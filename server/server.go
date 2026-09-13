// Package server hosts the MySQL wire protocol and SQL execution engine.
package server

import (
	"context"
	"errors"
	"fmt"

	mysqlserver "github.com/dolthub/go-mysql-server/server"
	"github.com/dolthub/go-mysql-server/sql"

	"github.com/nicbet/repodb/common/repository"
	"github.com/nicbet/repodb/engine"
)

type Config struct {
	Address      string
	DatabaseName string
	Repository   *repository.Repository
	Persistence  engine.PersistenceMode
}

type Server struct {
	wire   *mysqlserver.Server
	engine *engine.Engine
	repo   *repository.Repository
}

func New(config Config) (*Server, error) {
	if config.Repository == nil {
		return nil, errors.New("repository is required")
	}
	if config.Address == "" {
		config.Address = "127.0.0.1:3306"
	}
	if config.DatabaseName == "" {
		config.DatabaseName = "repodb"
	}

	persistent, err := engine.NewWithOptions(config.Repository, engine.Options{Persistence: config.Persistence})
	if err != nil {
		return nil, fmt.Errorf("open persistent SQL engine: %w", err)
	}
	wire, err := mysqlserver.NewServer(mysqlserver.Config{
		Protocol: "tcp",
		Address:  config.Address,
		Version:  "RepoDB experimental",
	}, persistent.SQLEngine(), sql.NewContext, persistent.SessionBuilder(), nil)
	if err != nil {
		_ = persistent.Close()
		return nil, fmt.Errorf("create MySQL server: %w", err)
	}
	return &Server{wire: wire, engine: persistent, repo: config.Repository}, nil
}

func (s *Server) Address() string { return s.wire.Listener.Addr().String() }

func (s *Server) Start() error { return s.wire.Start() }

func (s *Server) Close() error {
	wireErr := s.wire.Close()
	engineErr := s.engine.Close()
	if errors.Is(engineErr, context.Canceled) {
		engineErr = nil
	}
	if wireErr != nil && !errors.Is(wireErr, context.Canceled) {
		return errors.Join(wireErr, engineErr)
	}
	return engineErr
}

func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		if err := s.Close(); err != nil {
			return err
		}
		return nil
	}
}
