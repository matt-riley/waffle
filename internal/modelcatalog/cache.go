package modelcatalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matt-riley/waffle/internal/config"
)

const (
	SchemaVersion = 1
	DefaultTTL    = 24 * time.Hour
)

const staleRefreshWarning = "model catalogue refresh failed; using cached models"

type Record struct {
	SchemaVersion int        `json:"schema_version"`
	Connection    Connection `json:"connection"`
	FetchedAt     time.Time  `json:"fetched_at"`
	Models        []Model    `json:"models"`
}

type Result struct {
	Record
	Age     time.Duration `json:"-"`
	Stale   bool          `json:"stale"`
	Warning string        `json:"warning,omitempty"`
}

type Store struct {
	Root string
	TTL  time.Duration
	Now  func() time.Time
}

type cacheGeneration struct {
	identity string
}

func NewStore(home string) *Store {
	return &Store{
		Root: filepath.Join(home, "cache", "model-catalogs"),
		TTL:  DefaultTTL,
		Now:  time.Now,
	}
}

func (s *Store) Load(connection Connection) (Result, error) {
	result, _, err := s.load(connection)
	return result, err
}

func (s *Store) load(connection Connection) (Result, cacheGeneration, error) {
	if _, err := s.recordPath(connection.Name); err != nil {
		return Result{}, cacheGeneration{}, err
	}
	connection, err := normalizeConnection(connection)
	if err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("normalize model catalogue connection: %w", err)
	}
	directory, err := openSecureCacheDir(s.Root)
	if err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("open model catalogue cache directory: %w", err)
	}
	defer func() { _ = directory.close() }()
	return s.loadFromDir(directory, connection)
}

func (s *Store) loadFromDir(directory *secureCacheDir, connection Connection) (Result, cacheGeneration, error) {
	path, err := s.recordPath(connection.Name)
	if err != nil {
		return Result{}, cacheGeneration{}, err
	}
	wantConnection, err := normalizeConnection(connection)
	if err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("normalize model catalogue connection: %w", err)
	}

	file, generation, err := directory.openRegular(filepath.Base(path))
	if err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("open model catalogue cache: %w", err)
	}
	defer func() { _ = file.Close() }()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("decode model catalogue cache: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("decode model catalogue cache: %w", err)
	}
	if record.SchemaVersion != SchemaVersion {
		return Result{}, cacheGeneration{}, fmt.Errorf("model catalogue cache schema is %d, want %d", record.SchemaVersion, SchemaVersion)
	}
	gotConnection, err := normalizeConnection(record.Connection)
	if err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("normalize cached model catalogue connection: %w", err)
	}
	if gotConnection != wantConnection {
		return Result{}, cacheGeneration{}, errors.New("model catalogue cache connection does not match request")
	}
	record.Connection = gotConnection
	record.Models, err = Normalize(record.Models)
	if err != nil {
		return Result{}, cacheGeneration{}, fmt.Errorf("validate cached model catalogue: %w", err)
	}

	age := s.now().Sub(record.FetchedAt)
	if age < 0 {
		age = 0
	}
	return Result{
		Record: record,
		Age:    age,
		Stale:  age >= s.ttl(),
	}, generation, nil
}

func (s *Store) Save(connection Connection, models []Model, fetchedAt time.Time) error {
	if _, err := s.recordPath(connection.Name); err != nil {
		return err
	}
	connection, err := normalizeConnection(connection)
	if err != nil {
		return fmt.Errorf("normalize model catalogue connection: %w", err)
	}
	models, err = Normalize(models)
	if err != nil {
		return fmt.Errorf("normalize model catalogue: %w", err)
	}
	directory, err := s.ensureRoot()
	if err != nil {
		return err
	}
	defer func() { _ = directory.close() }()
	return s.saveInDir(directory, connection, models, fetchedAt)
}

func (s *Store) saveInDir(directory *secureCacheDir, connection Connection, models []Model, fetchedAt time.Time) error {
	path, err := s.recordPath(connection.Name)
	if err != nil {
		return err
	}
	connection, err = normalizeConnection(connection)
	if err != nil {
		return fmt.Errorf("normalize model catalogue connection: %w", err)
	}
	models, err = Normalize(models)
	if err != nil {
		return fmt.Errorf("normalize model catalogue: %w", err)
	}
	name := filepath.Base(path)
	if err := directory.validateMutationTarget(name); err != nil {
		return fmt.Errorf("validate existing model catalogue cache target: %w", err)
	}
	if err := directory.validate(); err != nil {
		return err
	}

	record := Record{
		SchemaVersion: SchemaVersion,
		Connection:    connection,
		FetchedAt:     fetchedAt,
		Models:        models,
	}
	temporary, err := os.CreateTemp(s.Root, "."+connection.Name+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create model catalogue cache staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure model catalogue cache staging file: %w", err)
	}
	temporaryGeneration, err := directory.validateTemporary(temporary, filepath.Base(temporaryPath))
	if err != nil {
		return fmt.Errorf("validate model catalogue cache staging file: %w", err)
	}
	if err := json.NewEncoder(temporary).Encode(record); err != nil {
		return fmt.Errorf("encode model catalogue cache: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync model catalogue cache staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close model catalogue cache staging file: %w", err)
	}
	renamed, err := directory.commitTemporary(filepath.Base(temporaryPath), name, temporaryGeneration)
	committed = renamed
	if err != nil {
		return fmt.Errorf("commit model catalogue cache: %w", err)
	}
	return nil
}

func (s *Store) GetOrRefresh(
	ctx context.Context,
	connection Connection,
	force bool,
	fetch func(context.Context) ([]Model, error),
) (Result, error) {
	if fetch == nil {
		return Result{}, errors.New("model catalogue refresh callback is nil")
	}
	if _, err := s.recordPath(connection.Name); err != nil {
		return Result{}, err
	}
	connection, err := normalizeConnection(connection)
	if err != nil {
		return Result{}, fmt.Errorf("normalize model catalogue connection: %w", err)
	}

	initial, initialGeneration, initialErr := s.load(connection)
	initialValid := initialErr == nil
	if initialValid && !force && !initial.Stale {
		return initial, nil
	}
	directory, err := s.ensureRoot()
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = directory.close() }()
	release, err := acquireRefreshLock(ctx, directory, connection.Name+".lock")
	if err != nil {
		return Result{}, fmt.Errorf("lock model catalogue refresh: %w", err)
	}
	defer func() { _ = release() }()

	current, currentGeneration, currentErr := s.loadFromDir(directory, connection)
	currentValid := currentErr == nil
	if currentValid {
		if !force && !current.Stale {
			return current, nil
		}
		if force && (!initialValid || currentGeneration != initialGeneration) {
			return current, nil
		}
	}

	models, refreshErr := fetch(ctx)
	if refreshErr == nil {
		models, refreshErr = Normalize(models)
	}
	if refreshErr != nil {
		if currentValid {
			current.Stale = true
			current.Warning = staleRefreshWarning
			return current, nil
		}
		return Result{}, refreshErr
	}

	fetchedAt := s.now()
	if err := s.saveInDir(directory, connection, models, fetchedAt); err != nil {
		return Result{}, err
	}
	normalizedConnection, err := normalizeConnection(connection)
	if err != nil {
		return Result{}, fmt.Errorf("normalize refreshed model catalogue connection: %w", err)
	}
	return Result{
		Record: Record{
			SchemaVersion: SchemaVersion,
			Connection:    normalizedConnection,
			FetchedAt:     fetchedAt,
			Models:        models,
		},
	}, nil
}

func (s *Store) Invalidate(connection string) error {
	path, err := s.recordPath(connection)
	if err != nil {
		return err
	}
	directory, err := openSecureCacheDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open model catalogue cache directory: %w", err)
	}
	defer func() { _ = directory.close() }()
	removed, err := directory.removeRegular(filepath.Base(path))
	if err != nil {
		return fmt.Errorf("remove model catalogue cache: %w", err)
	}
	if !removed {
		return nil
	}
	return nil
}

func (s *Store) recordPath(connection string) (string, error) {
	if !config.ValidProviderConnectionName(connection) {
		return "", fmt.Errorf("invalid connection name %q", connection)
	}
	if s.Root == "" {
		return "", errors.New("model catalogue cache root is empty")
	}
	return filepath.Join(s.Root, connection+".json"), nil
}

func (s *Store) ensureRoot() (*secureCacheDir, error) {
	if s.Root == "" {
		return nil, errors.New("model catalogue cache root is empty")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return nil, fmt.Errorf("create model catalogue cache directory: %w", err)
	}
	directory, err := secureCacheRoot(s.Root)
	if err != nil {
		return nil, fmt.Errorf("secure model catalogue cache directory: %w", err)
	}
	return directory, nil
}

func (s *Store) ttl() time.Duration {
	if s.TTL <= 0 {
		return DefaultTTL
	}
	return s.TTL
}

func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func normalizeConnection(connection Connection) (Connection, error) {
	if connection.ScopeID == "" {
		return Connection{}, errors.New("model catalogue connection scope ID is empty")
	}
	if connection.BaseURL == "" {
		return connection, nil
	}
	parsed, err := url.Parse(connection.BaseURL)
	if err != nil {
		return Connection{}, errors.New("model catalogue base URL is invalid")
	}
	if parsed.User != nil {
		return Connection{}, errors.New("model catalogue base URL must not contain userinfo")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return Connection{}, errors.New("model catalogue base URL must not contain a query")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return Connection{}, errors.New("model catalogue base URL must not contain a fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	connection.BaseURL = parsed.String()
	return connection, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("record has trailing JSON data")
	}
	return err
}
