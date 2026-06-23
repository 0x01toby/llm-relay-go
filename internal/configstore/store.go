// Package configstore holds the in-memory routing configuration: providers and
// model aliases, loaded from the database and cached for fast read access on
// the hot request path.
//
// It is a port of src/config.ts's caching layer. The original relied on
// module-level mutable variables (providerConfigs, aliasConfigs, uuidToChannelName)
// safe only because Node is single-threaded. The Go version stores the entire
// configuration in an immutable Snapshot behind an atomic.Pointer, so reads
// are lock-free and writes swap the whole snapshot atomically — no data races
// even with thousands of concurrent requests.
//
// Load coordination uses singleflight so the first N concurrent requests after
// a refresh don't each trigger a DB load.
package configstore

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"

	"github.com/taozhang/llmrelay/internal/repo"
)

// UpstreamType identifies a provider protocol family.
type UpstreamType string

const (
	Anthropic UpstreamType = "anthropic"
	OpenAI    UpstreamType = "openai"
)

// ResponsesMode controls how /v1/responses is handled for an OpenAI provider.
type ResponsesMode string

const (
	ResponsesNative     ResponsesMode = "native"
	ResponsesChatCompat ResponsesMode = "chat_compat"
	ResponsesDisabled   ResponsesMode = "disabled"
)

// DefaultResponsesMode is the default when unset.
const DefaultResponsesMode = ResponsesNative

// RoutingVisibility controls whether a provider participates in model routing.
type RoutingVisibility string

const (
	VisibilityDirect       RoutingVisibility = "direct"
	VisibilityExplicitOnly RoutingVisibility = "explicit_only"
)

// AuthConfig is a provider's upstream credential.
type AuthConfig struct {
	Header string // "x-api-key" or "authorization"
	Value  string
}

// ModelConfig is one entry in a provider's models list. Carries the model id
// plus optional context window and any extra fields (kept loosely typed).
type ModelConfig struct {
	Model   string                 `json:"model"`
	Context *int                   `json:"context,omitempty"`
	Extra   map[string]interface{} `json:"-"`
}

// ConfigEntry is the normalized in-memory representation of a provider.
// Mirrors ConfigEntry in config.ts.
type ConfigEntry struct {
	Type              UpstreamType
	TargetBaseURL     string
	SystemPrompt      string
	Auth              *AuthConfig
	Models            []ModelConfig
	Priority          int
	Enabled           bool
	RoutingVisibility RoutingVisibility
	ResponsesMode     ResponsesMode
	ExtraFields       map[string]interface{}
	ProviderUUID      string
}

// AliasTarget is one routing target for a virtual model (alias).
type AliasTarget struct {
	Provider string
	Model    string
}

// AliasEntry is the normalized in-memory representation of a model alias.
type AliasEntry struct {
	Alias    string
	Provider string
	Model    string
	Targets  []AliasTarget
	Visible  bool
	Enabled  bool
}

// Snapshot is an immutable point-in-time view of providers + aliases. Once
// published it is never mutated; a refresh builds a new one and atomically
// swaps it in. All routing reads operate against a *Snapshot.
type Snapshot struct {
	providers map[string]*ConfigEntry
	aliases   map[string]*AliasEntry
	uuidIndex map[string]string // providerUUID → channelName
}

// NewSnapshot builds an empty Snapshot (used for tests / boot-before-load).
func NewSnapshot() *Snapshot {
	return &Snapshot{
		providers: map[string]*ConfigEntry{},
		aliases:   map[string]*AliasEntry{},
		uuidIndex: map[string]string{},
	}
}

// Provider returns the entry for channelName, or nil.
func (s *Snapshot) Provider(channelName string) *ConfigEntry { return s.providers[channelName] }

// Providers returns all provider entries (do not mutate).
func (s *Snapshot) Providers() map[string]*ConfigEntry { return s.providers }

// Alias returns the alias entry for name, or nil.
func (s *Snapshot) Alias(name string) *AliasEntry { return s.aliases[name] }

// Aliases returns all alias entries (do not mutate).
func (s *Snapshot) Aliases() map[string]*AliasEntry { return s.aliases }

// ChannelForUUID resolves a provider UUID (or returns the input if not a UUID).
func (s *Snapshot) ChannelForUUID(uuid string) string {
	if name, ok := s.uuidIndex[uuid]; ok {
		return name
	}
	return uuid
}

// Empty reports whether the snapshot has no providers.
func (s *Snapshot) Empty() bool { return len(s.providers) == 0 }

// Store is the atomic config cache. Construct with NewStore and call
// EnsureLoaded at boot (and Refresh after mutations).
type Store struct {
	current  atomic.Pointer[Snapshot]
	loaded   atomic.Bool
	group    singleflight.Group
	provider *repo.ProviderRepo
	alias    *repo.AliasRepo
}

// NewStore builds a Store backed by the given repos. The initial snapshot is
// empty until EnsureLoaded succeeds.
func NewStore(providerRepo *repo.ProviderRepo, aliasRepo *repo.AliasRepo) *Store {
	s := &Store{provider: providerRepo, alias: aliasRepo}
	s.current.Store(NewSnapshot())
	return s
}

// NewStoreForPool is a convenience that builds the repos from a pool.
func NewStoreForPool(pool *pgxpool.Pool) *Store {
	return NewStore(repo.NewProviderRepo(pool), repo.NewAliasRepo(pool))
}

// Snapshot returns the current cached snapshot. It may be empty if loading
// has not yet run or failed; callers that need a populated view should call
// EnsureLoaded first.
func (s *Store) Snapshot() *Snapshot { return s.current.Load() }

// IsLoaded reports whether a successful load has completed.
func (s *Store) IsLoaded() bool { return s.loaded.Load() }

// EnsureLoaded populates the snapshot from the DB on first call; subsequent
// calls are no-ops until Refresh invalidates. Concurrent callers share a single
// load via singleflight (prevents thundering herd on boot).
func (s *Store) EnsureLoaded(ctx context.Context) error {
	if s.loaded.Load() {
		return nil
	}
	_, err, _ := s.group.Do("load", func() (interface{}, error) {
		if s.loaded.Load() {
			return nil, nil
		}
		snap, err := s.load(ctx)
		if err != nil {
			return nil, err
		}
		s.current.Store(snap)
		s.loaded.Store(true)
		return nil, nil
	})
	return err
}

// Refresh forces a reload from the DB, replacing the cached snapshot. Called
// after any admin mutation (create/update/delete provider or alias).
func (s *Store) Refresh(ctx context.Context) error {
	snap, err := s.load(ctx)
	if err != nil {
		return err
	}
	s.current.Store(snap)
	s.loaded.Store(true)
	return nil
}

// load queries both repos and assembles a fresh Snapshot.
func (s *Store) load(ctx context.Context) (*Snapshot, error) {
	providerRows, err := s.provider.List(ctx)
	if err != nil {
		return nil, err
	}
	aliasRows, err := s.alias.List(ctx)
	if err != nil {
		return nil, err
	}
	return buildSnapshot(providerRows, aliasRows), nil
}

// ResetForTest replaces the cache with an empty snapshot and clears the loaded
// flag. Test-only.
func (s *Store) ResetForTest() {
	s.current.Store(NewSnapshot())
	s.loaded.Store(false)
}

// LoadForTest installs a snapshot built from the given entries/aliases without
// touching the DB. Test-only.
func (s *Store) LoadForTest(providers map[string]*ConfigEntry, aliases map[string]*AliasEntry) {
	snap := NewSnapshot()
	for name, e := range providers {
		snap.providers[name] = e
		if e.ProviderUUID != "" {
			snap.uuidIndex[e.ProviderUUID] = name
		}
	}
	for name, a := range aliases {
		// Match buildSnapshot: disabled aliases are excluded from the cache so
		// routing never resolves to them.
		if !a.Enabled {
			continue
		}
		snap.aliases[name] = a
	}
	s.current.Store(snap)
	s.loaded.Store(true)
}

// ErrNotLoaded is returned when a snapshot is required but unavailable.
var ErrNotLoaded = errors.New("config not loaded")
