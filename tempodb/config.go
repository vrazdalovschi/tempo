package tempodb

import (
	"errors"
	"fmt"
	"time"

	"github.com/grafana/tempo/pkg/cache"
	"github.com/grafana/tempo/tempodb/backend/azure"
	"github.com/grafana/tempo/tempodb/backend/cache/memcached"
	"github.com/grafana/tempo/tempodb/backend/cache/redis"
	"github.com/grafana/tempo/tempodb/backend/gcs"
	"github.com/grafana/tempo/tempodb/backend/local"
	"github.com/grafana/tempo/tempodb/backend/s3"
	"github.com/grafana/tempo/tempodb/encoding"
	"github.com/grafana/tempo/tempodb/encoding/common"
	"github.com/grafana/tempo/tempodb/pool"
	"github.com/grafana/tempo/tempodb/wal"
)

const (
	DefaultBlocklistPoll            = 5 * time.Minute
	DefaultMaxTimePerTenant         = 5 * time.Minute
	DefaultBlocklistPollConcurrency = uint(50)
	DefaultRetentionConcurrency     = uint(10)
	DefaultTenantIndexBuilders      = 2
	DefaultPrefetchTraceCount       = 1000
	DefaultSearchChunkSizeBytes     = 1_000_000
)

// Config holds the entirety of tempodb configuration
// Defaults are in modules/storage/config.go
type Config struct {
	Pool   *pool.Config        `yaml:"pool,omitempty"`
	WAL    *wal.Config         `yaml:"wal"`
	Block  *common.BlockConfig `yaml:"block"`
	Search *SearchConfig       `yaml:"search"`

	BlocklistPoll                    time.Duration `yaml:"blocklist_poll"`
	BlocklistPollConcurrency         uint          `yaml:"blocklist_poll_concurrency"`
	BlocklistPollFallback            bool          `yaml:"blocklist_poll_fallback"`
	BlocklistPollTenantIndexBuilders int           `yaml:"blocklist_poll_tenant_index_builders"`
	BlocklistPollStaleTenantIndex    time.Duration `yaml:"blocklist_poll_stale_tenant_index"`
	BlocklistPollJitterMs            int           `yaml:"blocklist_poll_jitter_ms"`

	// backends
	Backend string        `yaml:"backend"`
	Local   *local.Config `yaml:"local"`
	GCS     *gcs.Config   `yaml:"gcs"`
	S3      *s3.Config    `yaml:"s3"`
	Azure   *azure.Config `yaml:"azure"`

	// caches
	Cache                   string                  `yaml:"cache"`
	CacheMinCompactionLevel uint8                   `yaml:"cache_min_compaction_level"`
	CacheMaxBlockAge        time.Duration           `yaml:"cache_max_block_age"`
	BackgroundCache         *cache.BackgroundConfig `yaml:"background_cache"`
	Memcached               *memcached.Config       `yaml:"memcached"`
	Redis                   *redis.Config           `yaml:"redis"`
}

type SearchConfig struct {
	ChunkSizeBytes     uint32 `yaml:"chunk_size_bytes"`
	PrefetchTraceCount int    `yaml:"prefetch_trace_count"`
}

// CompactorConfig contains compaction configuration options
type CompactorConfig struct {
	ChunkSizeBytes          uint32        `yaml:"chunk_size_bytes"`
	FlushSizeBytes          uint32        `yaml:"flush_size_bytes"`
	MaxCompactionRange      time.Duration `yaml:"compaction_window"`
	MaxCompactionObjects    int           `yaml:"max_compaction_objects"`
	MaxBlockBytes           uint64        `yaml:"max_block_bytes"`
	BlockRetention          time.Duration `yaml:"block_retention"`
	CompactedBlockRetention time.Duration `yaml:"compacted_block_retention"`
	RetentionConcurrency    uint          `yaml:"retention_concurrency"`
	IteratorBufferSize      int           `yaml:"iterator_buffer_size"`
	MaxTimePerTenant        time.Duration `yaml:"max_time_per_tenant"`
	CompactionCycle         time.Duration `yaml:"compaction_cycle"`
}

func validateConfig(cfg *Config) error {
	if cfg.WAL == nil {
		return errors.New("wal config should be non-nil")
	}

	if cfg.Block == nil {
		return errors.New("block config should be non-nil")
	}

	err := common.ValidateConfig(cfg.Block)
	if err != nil {
		return fmt.Errorf("block config validation failed: %w", err)
	}

	_, err = encoding.FromVersion(cfg.Block.Version)
	if err != nil {
		return fmt.Errorf("block version validation failed: %w", err)
	}

	return nil
}
