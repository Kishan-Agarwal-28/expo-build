package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// DB is the application database handle.
type DB struct {
	conn *sql.DB
}

// Build represents a single recorded build run.
type Build struct {
	ID        string    // appid-<unix-millis>
	AppID     string
	BuildType string    // Debug | Production | Signing Report
	Format    string    // APK | AAB | -
	Status    string    // running | success | failed
	Log       string
	ArtifactPath string
	CreatedAt time.Time
}

// Settings holds all configurable knobs for a given app.
type Settings struct {
	AppID            string
	S3BucketName     string
	AWSEndpoint      string
	AWSAccessKey     string
	AWSSecretKey     string
	AWSRegion        string
	BaseURL          string
	DeliveryMethod   string // local | s3
	GradleMaxHeap    string // e.g. "4g"
	GradleWorkers    string // e.g. "2"
	GradleParallel   bool
	GradleCaching    bool
	RNArchitectures  string // e.g. "armeabi-v7a,arm64-v8a"
	NodeMaxOldSpace  string // e.g. "8192"
	GitTracking      bool   // enable/disable version-control tracking
}

// DefaultSettings returns safe defaults.
func DefaultSettings(appID string) Settings {
	return Settings{
		AppID:           appID,
		DeliveryMethod:  "local",
		GradleMaxHeap:   "4g",
		GradleWorkers:   "2",
		GradleParallel:  true,
		GradleCaching:   true,
		RNArchitectures: "armeabi-v7a,arm64-v8a",
		NodeMaxOldSpace: "8192",
		GitTracking:     true,
	}
}

// Open opens (or creates) the SQLite database at the default path.
func Open() (*DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot locate home dir: %w", err)
	}
	dir := filepath.Join(home, ".expo-build")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cannot create data dir: %w", err)
	}
	dbPath := filepath.Join(dir, "expo-build.db")
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("cannot open sqlite: %w", err)
	}
	d := &DB{conn: conn}
	if err := d.migrate(); err != nil {
		conn.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error { return d.conn.Close() }

func (d *DB) migrate() error {
	_, err := d.conn.Exec(`
		CREATE TABLE IF NOT EXISTS builds (
			id            TEXT PRIMARY KEY,
			app_id        TEXT NOT NULL,
			build_type    TEXT NOT NULL,
			format        TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT 'running',
			log           TEXT NOT NULL DEFAULT '',
			artifact_path TEXT NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL
		);

		CREATE TABLE IF NOT EXISTS settings (
			app_id            TEXT PRIMARY KEY,
			s3_bucket_name    TEXT NOT NULL DEFAULT '',
			aws_endpoint      TEXT NOT NULL DEFAULT '',
			aws_access_key    TEXT NOT NULL DEFAULT '',
			aws_secret_key    TEXT NOT NULL DEFAULT '',
			aws_region        TEXT NOT NULL DEFAULT '',
			base_url          TEXT NOT NULL DEFAULT '',
			delivery_method   TEXT NOT NULL DEFAULT 'local',
			gradle_max_heap   TEXT NOT NULL DEFAULT '4g',
			gradle_workers    TEXT NOT NULL DEFAULT '2',
			gradle_parallel   INTEGER NOT NULL DEFAULT 1,
			gradle_caching    INTEGER NOT NULL DEFAULT 1,
			rn_architectures  TEXT NOT NULL DEFAULT 'armeabi-v7a,arm64-v8a',
			node_max_old_space TEXT NOT NULL DEFAULT '8192',
			git_tracking      INTEGER NOT NULL DEFAULT 1
		);
	`)
	return err
}

// --- Build CRUD ---

// InsertBuild inserts a new build record and returns the generated ID.
func (d *DB) InsertBuild(appID, buildType, format string) (string, error) {
	ts := time.Now()
	id := fmt.Sprintf("%s-%d", appID, ts.UnixMilli())
	_, err := d.conn.Exec(
		`INSERT INTO builds (id, app_id, build_type, format, status, log, created_at)
		 VALUES (?, ?, ?, ?, 'running', '', ?)`,
		id, appID, buildType, format, ts.Unix(),
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// AppendLog appends text to the build log.
func (d *DB) AppendLog(id, text string) error {
	_, err := d.conn.Exec(
		`UPDATE builds SET log = log || ? WHERE id = ?`, text, id,
	)
	return err
}

// SetBuildStatus updates the build status (and optionally the artifact path).
func (d *DB) SetBuildStatus(id, status, artifactPath string) error {
	_, err := d.conn.Exec(
		`UPDATE builds SET status = ?, artifact_path = ? WHERE id = ?`,
		status, artifactPath, id,
	)
	return err
}

// GetBuild retrieves a single build by ID.
func (d *DB) GetBuild(id string) (*Build, error) {
	row := d.conn.QueryRow(`SELECT id, app_id, build_type, format, status, log, artifact_path, created_at FROM builds WHERE id = ?`, id)
	return scanBuild(row)
}

// ListBuilds returns all builds for an app, newest first.
func (d *DB) ListBuilds(appID string) ([]Build, error) {
	rows, err := d.conn.Query(
		`SELECT id, app_id, build_type, format, status, log, artifact_path, created_at
		 FROM builds WHERE app_id = ? ORDER BY created_at DESC`, appID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var builds []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		builds = append(builds, *b)
	}
	return builds, rows.Err()
}

// ListAllBuilds returns every build across all apps, newest first.
func (d *DB) ListAllBuilds() ([]Build, error) {
	rows, err := d.conn.Query(
		`SELECT id, app_id, build_type, format, status, log, artifact_path, created_at
		 FROM builds ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var builds []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		builds = append(builds, *b)
	}
	return builds, rows.Err()
}

// ListAppIDs returns distinct app_ids ordered by most-recently-built first.
func (d *DB) ListAppIDs() ([]string, error) {
	rows, err := d.conn.Query(
		`SELECT app_id FROM builds GROUP BY app_id ORDER BY MAX(created_at) DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListSuccessfulBuilds returns builds with status="success" and a non-empty
// artifact_path, newest first — used by the Share tab.
func (d *DB) ListSuccessfulBuilds() ([]Build, error) {
	rows, err := d.conn.Query(
		`SELECT id, app_id, build_type, format, status, log, artifact_path, created_at
		 FROM builds WHERE status = 'success' AND artifact_path != ''
		 ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var builds []Build
	for rows.Next() {
		b, err := scanBuild(rows)
		if err != nil {
			return nil, err
		}
		builds = append(builds, *b)
	}
	return builds, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanBuild(s scanner) (*Build, error) {
	var b Build
	var unixSec int64
	if err := s.Scan(&b.ID, &b.AppID, &b.BuildType, &b.Format, &b.Status, &b.Log, &b.ArtifactPath, &unixSec); err != nil {
		return nil, err
	}
	b.CreatedAt = time.Unix(unixSec, 0)
	return &b, nil
}

// --- Settings CRUD ---

// GetSettings retrieves settings for an app, creating defaults if not present.
func (d *DB) GetSettings(appID string) (Settings, error) {
	row := d.conn.QueryRow(
		`SELECT app_id, s3_bucket_name, aws_endpoint, aws_access_key, aws_secret_key,
		        aws_region, base_url, delivery_method, gradle_max_heap, gradle_workers,
		        gradle_parallel, gradle_caching, rn_architectures, node_max_old_space, git_tracking
		 FROM settings WHERE app_id = ?`, appID,
	)
	var s Settings
	var gitTracking, parallel, caching int
	err := row.Scan(
		&s.AppID, &s.S3BucketName, &s.AWSEndpoint, &s.AWSAccessKey, &s.AWSSecretKey,
		&s.AWSRegion, &s.BaseURL, &s.DeliveryMethod, &s.GradleMaxHeap, &s.GradleWorkers,
		&parallel, &caching, &s.RNArchitectures, &s.NodeMaxOldSpace, &gitTracking,
	)
	if err == sql.ErrNoRows {
		def := DefaultSettings(appID)
		if saveErr := d.SaveSettings(def); saveErr != nil {
			return def, saveErr
		}
		return def, nil
	}
	if err != nil {
		return Settings{}, err
	}
	s.GitTracking = gitTracking != 0
	s.GradleParallel = parallel != 0
	s.GradleCaching = caching != 0
	return s, nil
}

// SaveSettings upserts the settings row for an app.
func (d *DB) SaveSettings(s Settings) error {
	gitTracking := 0
	if s.GitTracking {
		gitTracking = 1
	}
	parallel := 0
	if s.GradleParallel {
		parallel = 1
	}
	caching := 0
	if s.GradleCaching {
		caching = 1
	}
	_, err := d.conn.Exec(
		`INSERT INTO settings
		 (app_id, s3_bucket_name, aws_endpoint, aws_access_key, aws_secret_key,
		  aws_region, base_url, delivery_method, gradle_max_heap, gradle_workers,
		  gradle_parallel, gradle_caching, rn_architectures, node_max_old_space, git_tracking)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(app_id) DO UPDATE SET
		  s3_bucket_name=excluded.s3_bucket_name,
		  aws_endpoint=excluded.aws_endpoint,
		  aws_access_key=excluded.aws_access_key,
		  aws_secret_key=excluded.aws_secret_key,
		  aws_region=excluded.aws_region,
		  base_url=excluded.base_url,
		  delivery_method=excluded.delivery_method,
		  gradle_max_heap=excluded.gradle_max_heap,
		  gradle_workers=excluded.gradle_workers,
		  gradle_parallel=excluded.gradle_parallel,
		  gradle_caching=excluded.gradle_caching,
		  rn_architectures=excluded.rn_architectures,
		  node_max_old_space=excluded.node_max_old_space,
		  git_tracking=excluded.git_tracking`,
		s.AppID, s.S3BucketName, s.AWSEndpoint, s.AWSAccessKey, s.AWSSecretKey,
		s.AWSRegion, s.BaseURL, s.DeliveryMethod, s.GradleMaxHeap, s.GradleWorkers,
		parallel, caching, s.RNArchitectures, s.NodeMaxOldSpace, gitTracking,
	)
	return err
}
