package scale

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/dustin/go-humanize"
	"gopkg.in/yaml.v2"
)

type GarbageCollectionConfig struct {
	TTLSeconds uint `yaml:"ttlseconds"`
}

type ZoneConfig struct {
	RangeMinBytes     uint64                  `yaml:"range_min_bytes"`
	RangeMaxBytes     uint64                  `yaml:"range_max_bytes"`
	Replicas          uint                    `yaml:"num_replicas"`
	GarbageCollection GarbageCollectionConfig `yaml:"gc"`
}

func (c *ZoneConfig) Scan(value interface{}) error {
	bytes, ok := value.(string)
	if !ok {
		return errors.Errorf("expected string got %T", value)
	}

	return yaml.Unmarshal([]byte(bytes), c)
}

type Zone struct {
	Target string
	Config ZoneConfig
}

func ZoneConfigs(ctx context.Context, db *sql.DB) ([]Zone, error) {
	// TODO (chrisseto): Will we ever need additional fields??
	rows, err := db.QueryContext(ctx, `SELECT target, full_config_yaml FROM crdb_internal.zones`)
	if err != nil {
		return nil, errors.Wrap(err, "failed to select from crdb_internal.zones")
	}

	var zones []Zone

	for rows.Next() {
		var zone Zone

		if err := rows.Scan(&zone.Target, &zone.Config); err != nil {
			return nil, errors.Wrap(err, "failed to scan rows")
		}

		zones = append(zones, zone)
	}

	return zones, nil
}

func GetClusterSetting(ctx context.Context, db *sql.DB, name string) (string, error) {
	r := db.QueryRowContext(ctx, fmt.Sprintf("SHOW CLUSTER SETTING %s", name))
	var value string
	if err := r.Scan(&value); err != nil {
		return "", errors.Wrapf(err, "failed to get %s", name)
	}
	return value, nil
}

func SetClusterSetting(ctx context.Context, db *sql.DB, name, value string) error {
	sql := fmt.Sprintf("SET CLUSTER SETTING %s = $1", name)
	if _, err := db.Exec(sql, value); err != nil {
		return errors.Wrapf(err, "failed to set %s to %s", name, value)
	}
	return nil
}

// RangeMoveDuration calculates the slowest time.Duration that a range would
// reasonably take to move from one node to another.
// This duration does not account for IOPs or cluster load. If used as a timeout
// a multiple of this value should be used.
func RangeMoveDuration(ctx context.Context, db *sql.DB) (time.Duration, error) {
	rebalanceRate, err := GetClusterSetting(ctx, db, "kv.snapshot_rebalance.max_rate")
	if err != nil {
		return 0, errors.Wrap(err, "failed to get kv.snapshot_rebalance.max_rate")
	}

	recoveryRate, err := GetClusterSetting(ctx, db, "kv.snapshot_recovery.max_rate")
	if err != nil {
		return 0, errors.Wrap(err, "failed to get kv.snapshot_recovery.max_rate")
	}

	rebalanceBytes, err := humanize.ParseBytes(rebalanceRate)
	if err != nil {
		return 0, errors.Wrap(err, "failed to parse kv.snapshot_rebalance.max_rate as uint64")
	}

	recoveryBytes, err := humanize.ParseBytes(recoveryRate)
	if err != nil {
		return 0, errors.Wrap(err, "failed to parse kv.snapshot_recovery.max_rate as uint64")
	}

	// Get the slowest range moving rate
	minMoveSpeed := recoveryBytes
	if minMoveSpeed > rebalanceBytes {
		minMoveSpeed = rebalanceBytes
	}

	zones, err := ZoneConfigs(ctx, db)
	if err != nil {
		return 0, errors.Wrap(err, "failed to retrieve zone configs")
	}

	// Find the largest possible range size
	var maxRangeSize uint64
	for _, zone := range zones {
		if zone.Config.RangeMaxBytes > maxRangeSize {
			maxRangeSize = zone.Config.RangeMaxBytes
		}
	}

	if maxRangeSize == 0 {
		return 0, errors.New("no maximum range size found")
	}

	// Calculate the kindest (values wise, not respecting cluster load) possible duration
	// that it should take for a range to move from one node to another
	return time.Duration(maxRangeSize/minMoveSpeed) * time.Second, nil
}
