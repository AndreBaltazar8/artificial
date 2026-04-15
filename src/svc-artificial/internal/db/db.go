package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"artificial.pt/pkg-go-shared/protocol"

	_ "modernc.org/sqlite"
)

// DB wraps a SQLite database connection.
type DB struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path.
func Open(path string) (*DB, error) {
	conn, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	d := &DB{db: conn}
	if err := d.migrate(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

func (d *DB) migrate() error {
	// Create the migrations tracking table.
	if _, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS _migrations (id INTEGER PRIMARY KEY)`); err != nil {
		return err
	}

	for _, m := range migrations {
		var exists int
		d.db.QueryRow(`SELECT 1 FROM _migrations WHERE id = ?`, m.id).Scan(&exists)
		if exists == 1 {
			continue
		}
		if _, err := d.db.Exec(m.sql); err != nil {
			return fmt.Errorf("migration %d: %w", m.id, err)
		}
		if _, err := d.db.Exec(`INSERT INTO _migrations (id) VALUES (?)`, m.id); err != nil {
			return fmt.Errorf("record migration %d: %w", m.id, err)
		}
	}
	return nil
}

type migration struct {
	id  int
	sql string
}

// Append-only. Never edit or reorder existing entries.
var migrations = []migration{
	{1, `
		CREATE TABLE IF NOT EXISTS employees (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			nickname       TEXT UNIQUE NOT NULL,
			role           TEXT NOT NULL DEFAULT 'worker',
			persona        TEXT NOT NULL DEFAULT '',
			email          TEXT,
			employed       INTEGER NOT NULL DEFAULT 0,
			harness        TEXT NOT NULL DEFAULT 'claude',
			model          TEXT NOT NULL DEFAULT 'opus',
			acp_url        TEXT NOT NULL DEFAULT '',
			acp_provider   TEXT NOT NULL DEFAULT '',
			created_at     TEXT DEFAULT (datetime('now')),
			last_connected TEXT
		);
		CREATE TABLE IF NOT EXISTS projects (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT UNIQUE NOT NULL,
			path       TEXT,
			git_remote TEXT,
			created_at TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS channels (
			name   TEXT PRIMARY KEY,
			topic  TEXT NOT NULL DEFAULT '',
			set_by TEXT,
			set_at TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS channel_members (
			channel_name TEXT NOT NULL REFERENCES channels(name) ON DELETE CASCADE,
			employee_id  INTEGER NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
			joined_at    TEXT DEFAULT (datetime('now')),
			PRIMARY KEY (channel_name, employee_id)
		);
		CREATE TABLE IF NOT EXISTS messages (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			channel TEXT NOT NULL DEFAULT 'general',
			sender  TEXT NOT NULL,
			text    TEXT NOT NULL,
			ts      TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS tasks (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			title       TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			status      TEXT NOT NULL DEFAULT 'backlog',
			assignee    TEXT,
			project_id  INTEGER REFERENCES projects(id),
			created_by  TEXT NOT NULL,
			created_at  TEXT DEFAULT (datetime('now')),
			updated_at  TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS task_subscribers (
			task_id     INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			employee_id INTEGER NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
			PRIMARY KEY (task_id, employee_id)
		);
		CREATE TABLE IF NOT EXISTS workers (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			employee_id     INTEGER NOT NULL REFERENCES employees(id),
			pid             INTEGER,
			status          TEXT NOT NULL DEFAULT 'idle',
			session_id      TEXT,
			log_path        TEXT,
			transcript_path TEXT,
			created_at      TEXT DEFAULT (datetime('now')),
			last_connected  TEXT
		);
		CREATE TABLE IF NOT EXISTS read_cursors (
			employee_id  INTEGER NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
			channel_name TEXT NOT NULL,
			last_read_id INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (employee_id, channel_name)
		);
		CREATE TABLE IF NOT EXISTS worker_logs (
			id         TEXT PRIMARY KEY,
			worker_id  INTEGER NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
			path       TEXT NOT NULL,
			created_at TEXT DEFAULT (datetime('now'))
		);
		CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS reviews (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			worker_nick  TEXT NOT NULL,
			title        TEXT NOT NULL,
			description  TEXT NOT NULL DEFAULT '',
			type         TEXT NOT NULL DEFAULT 'choice',
			body         TEXT NOT NULL DEFAULT '{}',
			status       TEXT NOT NULL DEFAULT 'pending',
			response     TEXT,
			created_at   TEXT DEFAULT (datetime('now')),
			responded_at TEXT
		);
		INSERT OR IGNORE INTO channels (name, topic) VALUES ('general', 'General discussion');
	`},
	{2, `
		CREATE TABLE IF NOT EXISTS plugins (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT UNIQUE NOT NULL,
			path        TEXT NOT NULL,
			enabled     INTEGER NOT NULL DEFAULT 1,
			config_json TEXT NOT NULL DEFAULT '{}',
			created_at  TEXT DEFAULT (datetime('now'))
		);
	`},
	// Migration 3 adds the scope column so plugins can be either
	// host-spawned (svc-artificial owns the subprocess, every worker
	// reaches the plugin over the hub) or worker-spawned (each worker
	// has its own local subprocess, reserved for plugins that need
	// per-worker state). Existing rows default to "host" — that matches
	// the architectural direction the refactor takes: the default home
	// for any new plugin is svc-artificial.
	{3, `
		ALTER TABLE plugins ADD COLUMN scope TEXT NOT NULL DEFAULT 'host';
	`},
	// -- future migrations go here as {4, `...`}, etc.
}

func now() string { return time.Now().UTC().Format(time.DateTime) }

// ── Settings ────────────────────────────────────────────────────────────

func (d *DB) GetSetting(key string) (string, error) {
	var val string
	err := d.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return val, err
}

func (d *DB) SetSetting(key, value string) error {
	_, err := d.db.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

func (d *DB) GetAllSettings() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		rows.Scan(&k, &v)
		out[k] = v
	}
	return out, nil
}

// ── Employees ───────────────────────────────────────────────────────────

// CreateEmployee inserts a new employee and returns it with the generated ID.
func (d *DB) CreateEmployee(nickname, role, persona, email string) (protocol.Employee, error) {
	if role == "" {
		role = "worker"
	}
	ts := now()
	res, err := d.db.Exec(
		`INSERT INTO employees (nickname, role, persona, email, created_at) VALUES (?, ?, ?, ?, ?)`,
		nickname, role, persona, email, ts,
	)
	if err != nil {
		return protocol.Employee{}, err
	}
	id, _ := res.LastInsertId()
	return protocol.Employee{ID: id, Nickname: nickname, Role: role, Persona: persona, Email: email, CreatedAt: ts}, nil
}

// GetEmployee returns an employee by ID.
func (d *DB) GetEmployee(id int64) (protocol.Employee, error) {
	var e protocol.Employee
	var email, lastConn sql.NullString
	err := d.db.QueryRow(
		`SELECT id, nickname, role, persona, email, employed, harness, model, acp_url, acp_provider, created_at, last_connected FROM employees WHERE id = ?`, id,
	).Scan(&e.ID, &e.Nickname, &e.Role, &e.Persona, &email, &e.Employed, &e.Harness, &e.Model, &e.ACPURL, &e.ACPProvider, &e.CreatedAt, &lastConn)
	e.Email = email.String
	e.LastConnected = lastConn.String
	return e, err
}

// GetEmployeeByNick returns an employee by nickname.
func (d *DB) GetEmployeeByNick(nick string) (protocol.Employee, error) {
	var e protocol.Employee
	var email, lastConn sql.NullString
	err := d.db.QueryRow(
		`SELECT id, nickname, role, persona, email, employed, harness, model, acp_url, acp_provider, created_at, last_connected FROM employees WHERE nickname = ?`, nick,
	).Scan(&e.ID, &e.Nickname, &e.Role, &e.Persona, &email, &e.Employed, &e.Harness, &e.Model, &e.ACPURL, &e.ACPProvider, &e.CreatedAt, &lastConn)
	e.Email = email.String
	e.LastConnected = lastConn.String
	return e, err
}

// ListEmployees returns all employees.
func (d *DB) ListEmployees() ([]protocol.Employee, error) {
	rows, err := d.db.Query(`SELECT id, nickname, role, persona, email, employed, harness, model, acp_url, acp_provider, created_at, last_connected FROM employees ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Employee
	for rows.Next() {
		var e protocol.Employee
		var email, lastConn sql.NullString
		if err := rows.Scan(&e.ID, &e.Nickname, &e.Role, &e.Persona, &email, &e.Employed, &e.Harness, &e.Model, &e.ACPURL, &e.ACPProvider, &e.CreatedAt, &lastConn); err != nil {
			return nil, err
		}
		e.Email = email.String
		e.LastConnected = lastConn.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateEmployee updates an employee's mutable fields.
func (d *DB) UpdateEmployee(id int64, persona, email *string) error {
	if persona != nil {
		if _, err := d.db.Exec(`UPDATE employees SET persona = ? WHERE id = ?`, *persona, id); err != nil {
			return err
		}
	}
	if email != nil {
		if _, err := d.db.Exec(`UPDATE employees SET email = ? WHERE id = ?`, *email, id); err != nil {
			return err
		}
	}
	return nil
}

// UpdateEmployeeEmployed sets the employed flag for an employee.
func (d *DB) UpdateEmployeeEmployed(id int64, employed int) error {
	_, err := d.db.Exec(`UPDATE employees SET employed = ? WHERE id = ?`, employed, id)
	return err
}

// UpdateEmployeeHarness sets the harness, model, acp_url, and/or acp_provider for an employee.
func (d *DB) UpdateEmployeeHarness(id int64, harness, model, acpURL, acpProvider *string) error {
	if harness != nil {
		if _, err := d.db.Exec(`UPDATE employees SET harness = ? WHERE id = ?`, *harness, id); err != nil {
			return err
		}
	}
	if model != nil {
		if _, err := d.db.Exec(`UPDATE employees SET model = ? WHERE id = ?`, *model, id); err != nil {
			return err
		}
	}
	if acpURL != nil {
		if _, err := d.db.Exec(`UPDATE employees SET acp_url = ? WHERE id = ?`, *acpURL, id); err != nil {
			return err
		}
	}
	if acpProvider != nil {
		if _, err := d.db.Exec(`UPDATE employees SET acp_provider = ? WHERE id = ?`, *acpProvider, id); err != nil {
			return err
		}
	}
	return nil
}

// UpdateEmployeeLastConnected updates the last_connected timestamp for an employee.
func (d *DB) UpdateEmployeeLastConnected(id int64) error {
	_, err := d.db.Exec(`UPDATE employees SET last_connected = ? WHERE id = ?`, now(), id)
	return err
}

// ── Projects ────────────────────────────────────────────────────────────

// CreateProject creates a new project.
func (d *DB) CreateProject(name, path, gitRemote string) (protocol.Project, error) {
	ts := now()
	res, err := d.db.Exec(
		`INSERT INTO projects (name, path, git_remote, created_at) VALUES (?, ?, ?, ?)`,
		name, path, gitRemote, ts,
	)
	if err != nil {
		return protocol.Project{}, err
	}
	id, _ := res.LastInsertId()
	return protocol.Project{ID: id, Name: name, Path: path, GitRemote: gitRemote, CreatedAt: ts}, nil
}

// GetProject returns a project by ID.
func (d *DB) GetProject(id int64) (protocol.Project, error) {
	var p protocol.Project
	var path, remote sql.NullString
	err := d.db.QueryRow(
		`SELECT id, name, path, git_remote, created_at FROM projects WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &path, &remote, &p.CreatedAt)
	p.Path = path.String
	p.GitRemote = remote.String
	return p, err
}

// GetProjectByName returns a project by name.
func (d *DB) GetProjectByName(name string) (protocol.Project, error) {
	var p protocol.Project
	var path, remote sql.NullString
	err := d.db.QueryRow(
		`SELECT id, name, path, git_remote, created_at FROM projects WHERE name = ?`, name,
	).Scan(&p.ID, &p.Name, &path, &remote, &p.CreatedAt)
	p.Path = path.String
	p.GitRemote = remote.String
	return p, err
}

// ListProjects returns all projects.
func (d *DB) ListProjects() ([]protocol.Project, error) {
	rows, err := d.db.Query(`SELECT id, name, path, git_remote, created_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Project
	for rows.Next() {
		var p protocol.Project
		var path, remote sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &path, &remote, &p.CreatedAt); err != nil {
			return nil, err
		}
		p.Path = path.String
		p.GitRemote = remote.String
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteProject deletes a project by ID.
func (d *DB) DeleteProject(id int64) error {
	_, err := d.db.Exec(`DELETE FROM projects WHERE id = ?`, id)
	return err
}

// ── Channels ────────────────────────────────────────────────────────────

// CreateChannel creates a channel if it doesn't exist.
func (d *DB) CreateChannel(name string) error {
	_, err := d.db.Exec(`INSERT OR IGNORE INTO channels (name) VALUES (?)`, name)
	return err
}

// GetChannel returns a channel by name.
func (d *DB) GetChannel(name string) (protocol.Channel, error) {
	var c protocol.Channel
	var setBy, setAt sql.NullString
	err := d.db.QueryRow(`SELECT name, topic, set_by, set_at FROM channels WHERE name = ?`, name).
		Scan(&c.Name, &c.Topic, &setBy, &setAt)
	c.SetBy = setBy.String
	c.SetAt = setAt.String
	return c, err
}

// ListChannels returns all channels.
func (d *DB) ListChannels() ([]protocol.Channel, error) {
	rows, err := d.db.Query(`SELECT name, topic, set_by, set_at FROM channels ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Channel
	for rows.Next() {
		var c protocol.Channel
		var setBy, setAt sql.NullString
		if err := rows.Scan(&c.Name, &c.Topic, &setBy, &setAt); err != nil {
			return nil, err
		}
		c.SetBy = setBy.String
		c.SetAt = setAt.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetTopic updates a channel's topic.
func (d *DB) SetTopic(channel, topic, setBy string) error {
	_, err := d.db.Exec(
		`UPDATE channels SET topic = ?, set_by = ?, set_at = ? WHERE name = ?`,
		topic, setBy, now(), channel,
	)
	return err
}

// ── Channel Members ─────────────────────────────────────────────────────

// JoinChannel adds an employee to a channel. Creates the channel if needed.
func (d *DB) JoinChannel(channelName string, employeeID int64) error {
	if err := d.CreateChannel(channelName); err != nil {
		return err
	}
	_, err := d.db.Exec(
		`INSERT OR IGNORE INTO channel_members (channel_name, employee_id) VALUES (?, ?)`,
		channelName, employeeID,
	)
	return err
}

// LeaveChannel removes an employee from a channel.
func (d *DB) LeaveChannel(channelName string, employeeID int64) error {
	_, err := d.db.Exec(
		`DELETE FROM channel_members WHERE channel_name = ? AND employee_id = ?`,
		channelName, employeeID,
	)
	return err
}

// GetChannelMembers returns employee IDs in a channel.
func (d *DB) GetChannelMembers(channelName string) ([]int64, error) {
	rows, err := d.db.Query(`SELECT employee_id FROM channel_members WHERE channel_name = ?`, channelName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetChannelMemberNicks returns nicknames of all members in a channel.
func (d *DB) GetChannelMemberNicks(channelName string) ([]string, error) {
	rows, err := d.db.Query(
		`SELECT e.nickname FROM channel_members cm JOIN employees e ON e.id = cm.employee_id WHERE cm.channel_name = ?`,
		channelName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var nick string
		if err := rows.Scan(&nick); err != nil {
			return nil, err
		}
		out = append(out, nick)
	}
	return out, rows.Err()
}

// GetEmployeeChannels returns channel names an employee is a member of.
func (d *DB) GetEmployeeChannels(employeeID int64) ([]string, error) {
	rows, err := d.db.Query(`SELECT channel_name FROM channel_members WHERE employee_id = ?`, employeeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ── Messages ────────────────────────────────────────────────────────────

// SaveMessage saves a message and returns it with the generated ID and timestamp.
func (d *DB) SaveMessage(channel, sender, text string) (protocol.Message, error) {
	ts := now()
	res, err := d.db.Exec(
		`INSERT INTO messages (channel, sender, text, ts) VALUES (?, ?, ?, ?)`,
		channel, sender, text, ts,
	)
	if err != nil {
		return protocol.Message{}, err
	}
	id, _ := res.LastInsertId()
	return protocol.Message{ID: id, Channel: channel, Sender: sender, Text: text, TS: ts}, nil
}

// GetMessages returns messages in a channel, most recent N, ordered oldest first.
func (d *DB) GetMessages(channel string, limit int) ([]protocol.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.Query(
		`SELECT id, channel, sender, text, ts FROM messages WHERE channel = ? ORDER BY id DESC LIMIT ?`,
		channel, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Message
	for rows.Next() {
		var m protocol.Message
		if err := rows.Scan(&m.ID, &m.Channel, &m.Sender, &m.Text, &m.TS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	// Reverse so oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// GetMessagesBefore returns messages in a channel with id < beforeID, ordered oldest first.
// Used for backward pagination (loading older messages).
func (d *DB) GetMessagesBefore(channel string, beforeID int64, limit int) ([]protocol.Message, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.db.Query(
		`SELECT id, channel, sender, text, ts FROM messages WHERE channel = ? AND id < ? ORDER BY id DESC LIMIT ?`,
		channel, beforeID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Message
	for rows.Next() {
		var m protocol.Message
		if err := rows.Scan(&m.ID, &m.Channel, &m.Sender, &m.Text, &m.TS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	// Reverse so oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// GetMessagesSince returns messages in a channel with id > sinceID, ordered oldest first.
func (d *DB) GetMessagesSince(channel string, sinceID int64, limit int) ([]protocol.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.Query(
		`SELECT id, channel, sender, text, ts FROM messages WHERE channel = ? AND id > ? ORDER BY id ASC LIMIT ?`,
		channel, sinceID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Message
	for rows.Next() {
		var m protocol.Message
		if err := rows.Scan(&m.ID, &m.Channel, &m.Sender, &m.Text, &m.TS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetMessage returns a single message by ID.
func (d *DB) GetMessage(id int64) (protocol.Message, error) {
	var m protocol.Message
	err := d.db.QueryRow(
		`SELECT id, channel, sender, text, ts FROM messages WHERE id = ?`, id,
	).Scan(&m.ID, &m.Channel, &m.Sender, &m.Text, &m.TS)
	return m, err
}

// GrepMessages searches messages by text, optionally scoped to a channel and/or sender.
func (d *DB) GrepMessages(query, channel, sender string, limit int) ([]protocol.Message, error) {
	if limit <= 0 {
		limit = 20
	}
	q := `SELECT id, channel, sender, text, ts FROM messages WHERE text LIKE ?`
	args := []any{"%" + query + "%"}
	if channel != "" {
		q += ` AND channel = ?`
		args = append(args, channel)
	}
	if sender != "" {
		q += ` AND sender = ?`
		args = append(args, sender)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Message
	for rows.Next() {
		var m protocol.Message
		if err := rows.Scan(&m.ID, &m.Channel, &m.Sender, &m.Text, &m.TS); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetDMChannels returns all distinct DM channel names with their last message timestamp.
func (d *DB) GetDMChannels() ([]map[string]string, error) {
	rows, err := d.db.Query(
		`SELECT channel, MAX(ts) as last_ts FROM messages WHERE channel LIKE 'dm:%' GROUP BY channel ORDER BY last_ts DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var ch, ts string
		if err := rows.Scan(&ch, &ts); err != nil {
			return nil, err
		}
		out = append(out, map[string]string{"channel": ch, "last_ts": ts})
	}
	return out, rows.Err()
}

// ── Tasks ───────────────────────────────────────────────────────────────

// CreateTask inserts a new task.
func (d *DB) CreateTask(title, description, assignee string, projectID int64, createdBy string) (protocol.Task, error) {
	ts := now()
	var projID *int64
	if projectID > 0 {
		projID = &projectID
	}
	var assign *string
	if assignee != "" {
		assign = &assignee
	}
	res, err := d.db.Exec(
		`INSERT INTO tasks (title, description, assignee, project_id, created_by, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		title, description, assign, projID, createdBy, ts, ts,
	)
	if err != nil {
		return protocol.Task{}, err
	}
	id, _ := res.LastInsertId()
	return protocol.Task{
		ID: id, Title: title, Description: description, Status: "backlog",
		Assignee: assignee, ProjectID: projectID, CreatedBy: createdBy,
		CreatedAt: ts, UpdatedAt: ts,
	}, nil
}

// UpdateTask updates a task's status and/or assignee.
func (d *DB) UpdateTask(id int64, status, assignee *string) (protocol.Task, error) {
	ts := now()
	if status != nil {
		if _, err := d.db.Exec(`UPDATE tasks SET status = ?, updated_at = ? WHERE id = ?`, *status, ts, id); err != nil {
			return protocol.Task{}, err
		}
	}
	if assignee != nil {
		if _, err := d.db.Exec(`UPDATE tasks SET assignee = ?, updated_at = ? WHERE id = ?`, *assignee, ts, id); err != nil {
			return protocol.Task{}, err
		}
	}
	return d.GetTask(id)
}

// GetTask returns a task by ID.
func (d *DB) GetTask(id int64) (protocol.Task, error) {
	var t protocol.Task
	var desc, assignee sql.NullString
	var projID sql.NullInt64
	err := d.db.QueryRow(
		`SELECT id, title, description, status, assignee, project_id, created_by, created_at, updated_at FROM tasks WHERE id = ?`, id,
	).Scan(&t.ID, &t.Title, &desc, &t.Status, &assignee, &projID, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	t.Description = desc.String
	t.Assignee = assignee.String
	t.ProjectID = projID.Int64
	return t, err
}

// TaskListResult holds tasks and total count for paginated queries.
type TaskListResult struct {
	Tasks []protocol.Task `json:"tasks"`
	Total int             `json:"total"`
}

// ListTasks returns tasks, optionally filtered. Status supports comma-separated values.
// If limit > 0, results are capped and Total reflects the full count.
func (d *DB) ListTasks(status, assignee string, projectID int64, limit int) (TaskListResult, error) {
	where := ` WHERE 1=1`
	var args []any
	if status != "" {
		statuses := strings.Split(status, ",")
		placeholders := make([]string, len(statuses))
		for i, s := range statuses {
			placeholders[i] = "?"
			args = append(args, strings.TrimSpace(s))
		}
		where += ` AND status IN (` + strings.Join(placeholders, ",") + `)`
	}
	if assignee != "" {
		where += ` AND assignee = ?`
		args = append(args, assignee)
	}
	if projectID > 0 {
		where += ` AND project_id = ?`
		args = append(args, projectID)
	}

	// Get total count
	var total int
	d.db.QueryRow(`SELECT COUNT(*) FROM tasks`+where, args...).Scan(&total)

	query := `SELECT id, title, description, status, assignee, project_id, created_by, created_at, updated_at FROM tasks` + where + ` ORDER BY id`
	queryArgs := append([]any{}, args...)
	if limit > 0 {
		query += ` LIMIT ?`
		queryArgs = append(queryArgs, limit)
	}

	rows, err := d.db.Query(query, queryArgs...)
	if err != nil {
		return TaskListResult{}, err
	}
	defer rows.Close()
	var out []protocol.Task
	for rows.Next() {
		var t protocol.Task
		var desc, assign sql.NullString
		var projID sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Title, &desc, &t.Status, &assign, &projID, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return TaskListResult{}, err
		}
		t.Description = desc.String
		t.Assignee = assign.String
		t.ProjectID = projID.Int64
		out = append(out, t)
	}
	return TaskListResult{Tasks: out, Total: total}, rows.Err()
}

// GrepTasks searches tasks by title or description matching a query.
func (d *DB) GrepTasks(query string, projectID int64) ([]protocol.Task, error) {
	q := `SELECT id, title, description, status, assignee, project_id, created_by, created_at, updated_at
		FROM tasks WHERE (title LIKE ? OR description LIKE ?)`
	args := []any{"%" + query + "%", "%" + query + "%"}
	if projectID > 0 {
		q += ` AND project_id = ?`
		args = append(args, projectID)
	}
	q += ` ORDER BY updated_at DESC LIMIT 20`

	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Task
	for rows.Next() {
		var t protocol.Task
		var desc, assign sql.NullString
		var projID sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Title, &desc, &t.Status, &assign, &projID, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.Description = desc.String
		t.Assignee = assign.String
		t.ProjectID = projID.Int64
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTask deletes a task by ID.
func (d *DB) DeleteTask(id int64) error {
	_, err := d.db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
	return err
}

// ── Task Subscribers ────────────────────────────────────────────────────

// SubscribeToTask subscribes an employee to task updates.
func (d *DB) SubscribeToTask(taskID, employeeID int64) error {
	_, err := d.db.Exec(
		`INSERT OR IGNORE INTO task_subscribers (task_id, employee_id) VALUES (?, ?)`,
		taskID, employeeID,
	)
	return err
}

// GetTaskSubscribers returns employee IDs subscribed to a task.
func (d *DB) GetTaskSubscribers(taskID int64) ([]int64, error) {
	rows, err := d.db.Query(`SELECT employee_id FROM task_subscribers WHERE task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetTaskSubscriberNicks returns nicknames of employees subscribed to a task.
func (d *DB) GetTaskSubscriberNicks(taskID int64) ([]string, error) {
	rows, err := d.db.Query(
		`SELECT e.nickname FROM task_subscribers ts
		 JOIN employees e ON e.id = ts.employee_id
		 WHERE ts.task_id = ?`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var nick string
		if err := rows.Scan(&nick); err != nil {
			return nil, err
		}
		out = append(out, nick)
	}
	return out, rows.Err()
}

// ── Workers ─────────────────────────────────────────────────────────────

// CreateWorker registers a new worker process for an employee.
// Marks any existing non-offline workers for the same employee as offline first.
func (d *DB) CreateWorker(employeeID int64, pid int, logPath string) (protocol.Worker, error) {
	d.db.Exec(`UPDATE workers SET status = 'offline' WHERE employee_id = ? AND status != 'offline'`, employeeID)

	ts := now()
	res, err := d.db.Exec(
		`INSERT INTO workers (employee_id, pid, status, log_path, created_at, last_connected) VALUES (?, ?, 'online', ?, ?, ?)`,
		employeeID, pid, logPath, ts, ts,
	)
	if err != nil {
		return protocol.Worker{}, err
	}
	id, _ := res.LastInsertId()
	return protocol.Worker{ID: id, EmployeeID: employeeID, PID: pid, Status: "online", LogPath: logPath, CreatedAt: ts, LastConnected: ts}, nil
}

// UpdateWorkerTranscriptPath sets the transcript path on a worker.
func (d *DB) UpdateWorkerTranscriptPath(workerID int64, path string) error {
	_, err := d.db.Exec(`UPDATE workers SET transcript_path = ? WHERE id = ?`, path, workerID)
	return err
}

// UpdateWorkerPID sets the PID on a worker.
func (d *DB) UpdateWorkerPID(workerID int64, pid int) error {
	_, err := d.db.Exec(`UPDATE workers SET pid = ? WHERE id = ?`, pid, workerID)
	return err
}

// UpdateWorkerStatus updates a worker's status.
func (d *DB) UpdateWorkerStatus(workerID int64, status string) error {
	_, err := d.db.Exec(`UPDATE workers SET status = ?, last_connected = ? WHERE id = ?`, status, now(), workerID)
	return err
}

// UpdateWorkerSessionID stores the Claude session ID on a worker.
func (d *DB) UpdateWorkerSessionID(workerID int64, sessionID string) error {
	_, err := d.db.Exec(`UPDATE workers SET session_id = ? WHERE id = ?`, sessionID, workerID)
	return err
}

// GetWorker returns a worker by ID.
func (d *DB) GetWorker(id int64) (protocol.Worker, error) {
	var w protocol.Worker
	var sessID, logPath, transcriptPath, lastConn sql.NullString
	err := d.db.QueryRow(
		`SELECT id, employee_id, pid, status, session_id, log_path, transcript_path, created_at, last_connected FROM workers WHERE id = ?`, id,
	).Scan(&w.ID, &w.EmployeeID, &w.PID, &w.Status, &sessID, &logPath, &transcriptPath, &w.CreatedAt, &lastConn)
	w.SessionID = sessID.String
	w.LogPath = logPath.String
	w.TranscriptPath = transcriptPath.String
	w.LastConnected = lastConn.String
	return w, err
}

// ListWorkers returns the latest worker per employee (skips old offline entries).
func (d *DB) ListWorkers() ([]protocol.Worker, error) {
	rows, err := d.db.Query(
		`SELECT id, employee_id, pid, status, session_id, log_path, transcript_path, created_at, last_connected
		 FROM workers
		 WHERE id IN (SELECT MAX(id) FROM workers GROUP BY employee_id)
		 ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Worker
	for rows.Next() {
		var w protocol.Worker
		var sessID, logPath, transcriptPath, lastConn sql.NullString
		if err := rows.Scan(&w.ID, &w.EmployeeID, &w.PID, &w.Status, &sessID, &logPath, &transcriptPath, &w.CreatedAt, &lastConn); err != nil {
			return nil, err
		}
		w.SessionID = sessID.String
		w.LogPath = logPath.String
		w.TranscriptPath = transcriptPath.String
		w.LastConnected = lastConn.String
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetLatestWorkerForEmployee returns the most recent worker for an employee, if any.
func (d *DB) GetLatestWorkerForEmployee(employeeID int64) (protocol.Worker, bool) {
	var w protocol.Worker
	var sessID, logPath, lastConn sql.NullString
	err := d.db.QueryRow(
		`SELECT id, employee_id, pid, status, session_id, log_path, created_at, last_connected
		 FROM workers WHERE employee_id = ? ORDER BY id DESC LIMIT 1`, employeeID,
	).Scan(&w.ID, &w.EmployeeID, &w.PID, &w.Status, &sessID, &logPath, &w.CreatedAt, &lastConn)
	if err != nil {
		return w, false
	}
	w.SessionID = sessID.String
	w.LogPath = logPath.String
	w.LastConnected = lastConn.String
	return w, true
}

// ── Read Cursors ────────────────────────────────────────────────────────

// SetReadCursor updates the last-read message ID for an employee in a channel/DM.
func (d *DB) SetReadCursor(employeeID int64, channelName string, lastReadID int64) error {
	_, err := d.db.Exec(
		`INSERT INTO read_cursors (employee_id, channel_name, last_read_id) VALUES (?, ?, ?)
		 ON CONFLICT(employee_id, channel_name) DO UPDATE SET last_read_id = excluded.last_read_id`,
		employeeID, channelName, lastReadID,
	)
	return err
}

// GetReadCursor returns the last-read message ID for an employee in a channel.
func (d *DB) GetReadCursor(employeeID int64, channelName string) (int64, error) {
	var id int64
	err := d.db.QueryRow(
		`SELECT last_read_id FROM read_cursors WHERE employee_id = ? AND channel_name = ?`,
		employeeID, channelName,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// GetUnreadCounts returns a map of channel_name → unread count for an employee.
// Includes channels they're a member of, general, and any DMs they're part of.
func (d *DB) GetUnreadCounts(employeeID int64) (map[string]int, error) {
	// Get the employee's nickname for DM matching.
	var nick string
	if err := d.db.QueryRow(`SELECT nickname FROM employees WHERE id = ?`, employeeID).Scan(&nick); err != nil {
		return nil, err
	}

	rows, err := d.db.Query(`
		SELECT m.channel, COUNT(*)
		FROM messages m
		LEFT JOIN read_cursors rc ON rc.employee_id = ? AND rc.channel_name = m.channel
		WHERE m.id > COALESCE(rc.last_read_id, 0)
		  AND (
			m.channel IN (SELECT channel_name FROM channel_members WHERE employee_id = ?)
			OR m.channel = 'general'
			OR m.channel LIKE '%' || ? || '%'
		  )
		GROUP BY m.channel
	`, employeeID, employeeID, nick)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var ch string
		var count int
		if err := rows.Scan(&ch, &count); err != nil {
			return nil, err
		}
		out[ch] = count
	}
	return out, rows.Err()
}

// ── Worker Logs ─────────────────────────────────────────────────────────

// WorkerLog represents a log file associated with a worker.
type WorkerLog struct {
	ID        string `json:"id"`
	WorkerID  int64  `json:"worker_id"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
}

// CreateWorkerLog records a log file for a worker.
func (d *DB) CreateWorkerLog(id string, workerID int64, path string) error {
	_, err := d.db.Exec(
		`INSERT INTO worker_logs (id, worker_id, path, created_at) VALUES (?, ?, ?, ?)`,
		id, workerID, path, now(),
	)
	return err
}

// ── Reviews ─────────────────────────────────────────────────────────────

func (d *DB) CreateReview(workerNick, title, description, reviewType, body string) (protocol.Review, error) {
	ts := now()
	res, err := d.db.Exec(
		`INSERT INTO reviews (worker_nick, title, description, type, body, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		workerNick, title, description, reviewType, body, ts,
	)
	if err != nil {
		return protocol.Review{}, err
	}
	id, _ := res.LastInsertId()
	return protocol.Review{
		ID: id, WorkerNick: workerNick, Title: title, Description: description,
		Type: reviewType, Body: body, Status: "pending", CreatedAt: ts,
	}, nil
}

func (d *DB) RespondToReview(id int64, response string) (protocol.Review, error) {
	ts := now()
	_, err := d.db.Exec(
		`UPDATE reviews SET status = 'responded', response = ?, responded_at = ? WHERE id = ?`,
		response, ts, id,
	)
	if err != nil {
		return protocol.Review{}, err
	}
	return d.GetReview(id)
}

func (d *DB) GetReview(id int64) (protocol.Review, error) {
	var r protocol.Review
	var desc, response, respondedAt sql.NullString
	err := d.db.QueryRow(
		`SELECT id, worker_nick, title, description, type, body, status, response, created_at, responded_at FROM reviews WHERE id = ?`, id,
	).Scan(&r.ID, &r.WorkerNick, &r.Title, &desc, &r.Type, &r.Body, &r.Status, &response, &r.CreatedAt, &respondedAt)
	r.Description = desc.String
	r.Response = response.String
	r.RespondedAt = respondedAt.String
	return r, err
}

func (d *DB) ListPendingReviews() ([]protocol.Review, error) {
	rows, err := d.db.Query(
		`SELECT id, worker_nick, title, description, type, body, status, response, created_at, responded_at FROM reviews WHERE status = 'pending' ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Review
	for rows.Next() {
		var r protocol.Review
		var desc, response, respondedAt sql.NullString
		if err := rows.Scan(&r.ID, &r.WorkerNick, &r.Title, &desc, &r.Type, &r.Body, &r.Status, &response, &r.CreatedAt, &respondedAt); err != nil {
			return nil, err
		}
		r.Description = desc.String
		r.Response = response.String
		r.RespondedAt = respondedAt.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetWorkerLogs returns all log files for a worker.
func (d *DB) GetWorkerLogs(workerID int64) ([]WorkerLog, error) {
	rows, err := d.db.Query(
		`SELECT id, worker_id, path, created_at FROM worker_logs WHERE worker_id = ? ORDER BY created_at`,
		workerID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerLog
	for rows.Next() {
		var l WorkerLog
		if err := rows.Scan(&l.ID, &l.WorkerID, &l.Path, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ── Plugins ─────────────────────────────────────────────────────────────
//
// Plugin rows only persist the static config: name (the stable identifier
// matching ArtificialPlugin.Name()), path (filesystem path to the plugin
// binary), enabled flag, and a JSON config blob plugin authors can read at
// load time. Runtime state (which workers have the plugin loaded, which
// tools are currently registered, last load error) is NOT stored here —
// it lives in Hub in-memory aggregation populated by MsgWorkerPluginState
// reports from workers. Query paths that surface a Plugin to the dashboard
// must layer that runtime state on top of what these methods return.

// ListPlugins returns every plugin row, ordered by name.
func (d *DB) ListPlugins() ([]protocol.Plugin, error) {
	rows, err := d.db.Query(
		`SELECT id, name, path, enabled, scope, config_json, created_at FROM plugins ORDER BY name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []protocol.Plugin
	for rows.Next() {
		p, err := scanPlugin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPlugin fetches a single plugin by id. Returns sql.ErrNoRows if the
// id is unknown — callers should translate that to a 404.
func (d *DB) GetPlugin(id int64) (protocol.Plugin, error) {
	row := d.db.QueryRow(
		`SELECT id, name, path, enabled, scope, config_json, created_at FROM plugins WHERE id = ?`,
		id,
	)
	return scanPlugin(row)
}

// GetPluginByName is the workhorse the pluginhost loader calls to
// resolve a plugin name to its binary path + config. Returns
// sql.ErrNoRows when the plugin is absent.
func (d *DB) GetPluginByName(name string) (protocol.Plugin, error) {
	row := d.db.QueryRow(
		`SELECT id, name, path, enabled, scope, config_json, created_at FROM plugins WHERE name = ?`,
		name,
	)
	return scanPlugin(row)
}

// UpsertPlugin creates a plugin row, or updates an existing row if one
// already exists with the same name. Used by POST /api/plugins from the
// dashboard. configJSON must be a valid JSON document or "{}" — an empty
// string is normalised to "{}" to satisfy the NOT NULL constraint.
// Empty scope folds to PluginScopeHost so the default new-plugin home
// is svc-artificial.
func (d *DB) UpsertPlugin(name, path string, enabled bool, scope, configJSON string) (protocol.Plugin, error) {
	if configJSON == "" {
		configJSON = "{}"
	}
	scope = protocol.NormalizePluginScope(scope)
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	_, err := d.db.Exec(
		`INSERT INTO plugins (name, path, enabled, scope, config_json) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET path = excluded.path, enabled = excluded.enabled, scope = excluded.scope, config_json = excluded.config_json`,
		name, path, enabledInt, scope, configJSON,
	)
	if err != nil {
		return protocol.Plugin{}, err
	}
	return d.GetPluginByName(name)
}

// SetPluginScope flips the scope between host and worker and returns
// the updated row. Caller should broadcast MsgPluginChanged so both
// svc-artificial's host loader and every worker re-reconcile their
// local pluginhosts against the new scope column.
func (d *DB) SetPluginScope(id int64, scope string) (protocol.Plugin, error) {
	scope = protocol.NormalizePluginScope(scope)
	if _, err := d.db.Exec(`UPDATE plugins SET scope = ? WHERE id = ?`, scope, id); err != nil {
		return protocol.Plugin{}, err
	}
	return d.GetPlugin(id)
}

// SetPluginEnabled flips the enabled flag for a plugin. The dashboard
// toggle button hits this path via PATCH /api/plugins/{id}.
func (d *DB) SetPluginEnabled(id int64, enabled bool) (protocol.Plugin, error) {
	enabledInt := 0
	if enabled {
		enabledInt = 1
	}
	if _, err := d.db.Exec(`UPDATE plugins SET enabled = ? WHERE id = ?`, enabledInt, id); err != nil {
		return protocol.Plugin{}, err
	}
	return d.GetPlugin(id)
}

// SetPluginConfig replaces the config JSON blob for a plugin. Caller is
// responsible for validating JSON shape before invoking.
func (d *DB) SetPluginConfig(id int64, configJSON string) (protocol.Plugin, error) {
	if configJSON == "" {
		configJSON = "{}"
	}
	if _, err := d.db.Exec(`UPDATE plugins SET config_json = ? WHERE id = ?`, configJSON, id); err != nil {
		return protocol.Plugin{}, err
	}
	return d.GetPlugin(id)
}

// DeletePlugin removes a plugin row. No cascade — workers that already
// loaded the plugin keep running with it until they reload; the Hub
// aggregator is responsible for garbage-collecting stale runtime state.
func (d *DB) DeletePlugin(id int64) error {
	_, err := d.db.Exec(`DELETE FROM plugins WHERE id = ?`, id)
	return err
}

// scanPlugin is a small helper shared by the row-at-a-time query paths.
// It lifts the shared Scan logic out of each call site so the JSON
// config parse happens in exactly one place.
func scanPlugin(r interface {
	Scan(dest ...any) error
}) (protocol.Plugin, error) {
	var (
		p          protocol.Plugin
		enabledInt int
		scope      string
		configJSON string
	)
	if err := r.Scan(&p.ID, &p.Name, &p.Path, &enabledInt, &scope, &configJSON, &p.CreatedAt); err != nil {
		return protocol.Plugin{}, err
	}
	p.Enabled = enabledInt == 1
	p.Scope = protocol.NormalizePluginScope(scope)
	if configJSON != "" && configJSON != "{}" {
		// Best-effort parse — a malformed config does not stop us from
		// returning the row; the dashboard renders it as a raw string.
		var parsed any
		if err := json.Unmarshal([]byte(configJSON), &parsed); err == nil {
			p.Config = parsed
		} else {
			p.Config = configJSON
		}
	}
	return p, nil
}
