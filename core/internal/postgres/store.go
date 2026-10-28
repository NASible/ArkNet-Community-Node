// Package postgres implements the internal node repository using PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"arknet/core/internal/nodes"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type Store struct{ db *sql.DB }

var _ nodes.Repository = (*Store)(nil)

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgx.ParseConfig(dsn)
	// Connection strings can contain secrets; do not propagate parser errors.
	if err != nil {
		return nil, errors.New("invalid database configuration")
	}
	cfg.ConnectTimeout = 5 * time.Second
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, errors.New("database connection failed; check PostgreSQL and connection configuration")
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error                   { return s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) SQLDB() *sql.DB                 { return s.db }

const columns = "id, public_id, enrollment_status, trust_status, software_version, created_at, last_seen_at, current_state, reported_health"

type scanner interface{ Scan(...any) error }

func scanNode(row scanner) (nodes.Node, error) {
	var node nodes.Node
	var health []byte
	err := row.Scan(&node.ID, &node.PublicID, &node.EnrollmentStatus, &node.TrustStatus,
		&node.SoftwareVersion, &node.CreatedAt, &node.LastSeenAt, &node.CurrentState, &health)
	if err == nil {
		err = decodeHealth(&node, health)
	}
	return node, err
}

func decodeHealth(node *nodes.Node, raw []byte) error {
	if raw == nil {
		return nil
	}
	node.ReportedHealth = &nodes.BasicHealth{}
	return json.Unmarshal(raw, node.ReportedHealth)
}

func (s *Store) Create(ctx context.Context, softwareVersion string) (nodes.Node, error) {
	if strings.TrimSpace(softwareVersion) == "" {
		return nodes.Node{}, nodes.ErrInvalidSoftwareVersion
	}
	id, err := nodes.NewPublicID()
	if err != nil {
		return nodes.Node{}, fmt.Errorf("generate public node ID: %w", err)
	}
	node, err := scanNode(s.db.QueryRowContext(ctx,
		"INSERT INTO nodes (public_id, enrollment_status, trust_status, software_version) VALUES ($1, $2, $3, $4) RETURNING "+columns,
		id, nodes.EnrollmentPending, nodes.TrustUntrusted, softwareVersion))
	if err != nil {
		return nodes.Node{}, fmt.Errorf("create node: %w", err)
	}
	return node, nil
}

func (s *Store) GetByPublicID(ctx context.Context, publicID string) (nodes.Node, error) {
	if !nodes.ValidPublicID(publicID) {
		return nodes.Node{}, nodes.ErrInvalidPublicID
	}
	node, err := scanNode(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM nodes WHERE public_id = $1", publicID))
	if errors.Is(err, sql.ErrNoRows) {
		return nodes.Node{}, nodes.ErrNotFound
	}
	if err != nil {
		return nodes.Node{}, fmt.Errorf("read node: %w", err)
	}
	return node, nil
}
