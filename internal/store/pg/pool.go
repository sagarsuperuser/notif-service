package pg

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type PoolOptions struct {
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   string
	MaxConnIdleTime   string
	HealthCheckPeriod string
}

func NewPool(ctx context.Context, dsn string, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}

	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns >= 0 {
		cfg.MinConns = opts.MinConns
	}

	if opts.MaxConnLifetime != "" {
		d, err := time.ParseDuration(opts.MaxConnLifetime)
		if err != nil {
			return nil, fmt.Errorf("invalid DB_POOL_MAX_CONN_LIFETIME: %w", err)
		}
		cfg.MaxConnLifetime = d
	}
	if opts.MaxConnIdleTime != "" {
		d, err := time.ParseDuration(opts.MaxConnIdleTime)
		if err != nil {
			return nil, fmt.Errorf("invalid DB_POOL_MAX_CONN_IDLE_TIME: %w", err)
		}
		cfg.MaxConnIdleTime = d
	}
	if opts.HealthCheckPeriod != "" {
		d, err := time.ParseDuration(opts.HealthCheckPeriod)
		if err != nil {
			return nil, fmt.Errorf("invalid DB_POOL_HEALTH_CHECK_PERIOD: %w", err)
		}
		cfg.HealthCheckPeriod = d
	}

	return pgxpool.NewWithConfig(ctx, cfg)
}
