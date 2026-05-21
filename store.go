package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type ConversionRecord struct {
	ID           int64     `json:"id"`
	SourcePath   string    `json:"source_path"`
	TargetPath   string    `json:"target_path"`
	SourceFormat string    `json:"source_format"`
	TargetFormat string    `json:"target_format"`
	SourceSize   int64     `json:"source_size"`
	TargetSize   int64     `json:"target_size"`
	Status       string    `json:"status"`
	Error        string    `json:"error"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	BackupPath   string    `json:"backup_path"`
}

type WatchedFolder struct {
	ID      int64     `json:"id"`
	Path    string    `json:"path"`
	Enabled bool      `json:"enabled"`
	AddedAt time.Time `json:"added_at"`
}

func OpenStore(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS conversions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_path TEXT, target_path TEXT,
			source_format TEXT, target_format TEXT,
			source_size INTEGER, target_size INTEGER,
			status TEXT,
			error TEXT,
			started_at DATETIME, finished_at DATETIME,
			backup_path TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS watched_folders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT UNIQUE,
			enabled INTEGER DEFAULT 1,
			added_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) InsertConversion(c *ConversionRecord) error {
	res, err := s.db.Exec(`INSERT INTO conversions
		(source_path,target_path,source_format,target_format,source_size,target_size,status,error,started_at,finished_at,backup_path)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		c.SourcePath, c.TargetPath, c.SourceFormat, c.TargetFormat,
		c.SourceSize, c.TargetSize, c.Status, c.Error,
		c.StartedAt, c.FinishedAt, c.BackupPath)
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	c.ID = id
	return nil
}

func (s *Store) ListConversions(limit int) ([]ConversionRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,source_path,target_path,source_format,target_format,
		source_size,target_size,status,error,started_at,finished_at,backup_path
		FROM conversions ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversionRecord
	for rows.Next() {
		var c ConversionRecord
		if err := rows.Scan(&c.ID, &c.SourcePath, &c.TargetPath, &c.SourceFormat, &c.TargetFormat,
			&c.SourceSize, &c.TargetSize, &c.Status, &c.Error, &c.StartedAt, &c.FinishedAt, &c.BackupPath); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConversion returns a single conversion record by id.
func (s *Store) GetConversion(id int64) (*ConversionRecord, error) {
	row := s.db.QueryRow(`SELECT id,source_path,target_path,source_format,target_format,
		source_size,target_size,status,error,started_at,finished_at,backup_path
		FROM conversions WHERE id=?`, id)
	var c ConversionRecord
	if err := row.Scan(&c.ID, &c.SourcePath, &c.TargetPath, &c.SourceFormat, &c.TargetFormat,
		&c.SourceSize, &c.TargetSize, &c.Status, &c.Error, &c.StartedAt, &c.FinishedAt, &c.BackupPath); err != nil {
		return nil, err
	}
	return &c, nil
}

// ListBackups returns the most recent conversions that have a non-empty
// backup_path, regardless of conversion status (failed conversions also keep
// backups since the original was restored from one).
func (s *Store) ListBackups(limit int) ([]ConversionRecord, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT id,source_path,target_path,source_format,target_format,
		source_size,target_size,status,error,started_at,finished_at,backup_path
		FROM conversions WHERE backup_path <> '' ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConversionRecord
	for rows.Next() {
		var c ConversionRecord
		if err := rows.Scan(&c.ID, &c.SourcePath, &c.TargetPath, &c.SourceFormat, &c.TargetFormat,
			&c.SourceSize, &c.TargetSize, &c.Status, &c.Error, &c.StartedAt, &c.FinishedAt, &c.BackupPath); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) AnalyticsTotals() (totalConversions int64, bytesSaved int64, err error) {
	row := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(source_size - target_size),0)
		FROM conversions WHERE status='completed'`)
	err = row.Scan(&totalConversions, &bytesSaved)
	return
}

func (s *Store) AddFolder(path string) (*WatchedFolder, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO watched_folders (path,enabled,added_at) VALUES (?,1,?)`, path, now)
	if err != nil {
		return nil, err
	}
	row := s.db.QueryRow(`SELECT id,path,enabled,added_at FROM watched_folders WHERE path=?`, path)
	wf := &WatchedFolder{}
	var enabled int
	if err := row.Scan(&wf.ID, &wf.Path, &enabled, &wf.AddedAt); err != nil {
		return nil, err
	}
	wf.Enabled = enabled == 1
	return wf, nil
}

func (s *Store) RemoveFolder(id int64) error {
	_, err := s.db.Exec(`DELETE FROM watched_folders WHERE id=?`, id)
	return err
}

func (s *Store) SetFolderEnabled(id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE watched_folders SET enabled=? WHERE id=?`, v, id)
	return err
}

func (s *Store) ListFolders() ([]WatchedFolder, error) {
	rows, err := s.db.Query(`SELECT id,path,enabled,added_at FROM watched_folders ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WatchedFolder
	for rows.Next() {
		var wf WatchedFolder
		var enabled int
		if err := rows.Scan(&wf.ID, &wf.Path, &enabled, &wf.AddedAt); err != nil {
			return nil, err
		}
		wf.Enabled = enabled == 1
		out = append(out, wf)
	}
	return out, rows.Err()
}

func (s *Store) GetSetting(key string, dst interface{}) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if dst == nil {
		return true, nil
	}
	return true, json.Unmarshal([]byte(raw), dst)
}

func (s *Store) SetSetting(key string, val interface{}) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(raw))
	return err
}
