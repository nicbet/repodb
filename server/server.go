// Package server hosts the MySQL wire protocol and SQL execution engine.
package server

import (
	"context"
	"errors"
	"fmt"

	sqle "github.com/dolthub/go-mysql-server"
	"github.com/dolthub/go-mysql-server/memory"
	mysqlserver "github.com/dolthub/go-mysql-server/server"
	"github.com/dolthub/go-mysql-server/sql"

	"github.com/nicbet/repodb/common/repository"
)

type Config struct {
	Address      string
	DatabaseName string
	Repository   *repository.Repository
}

type Server struct {
	wire   *mysqlserver.Server
	engine *sqle.Engine
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

	// The memory catalog is the first SQL adapter. The next storage milestone is
	// to replace its tables with sql.Table implementations backed by the Prolly
	// roots in config.Repository's manifest.
	db := memory.NewDatabase(config.DatabaseName)
	db.BaseDatabase.EnablePrimaryKeyIndexes()
	provider := memory.NewDBProvider(db)
	engine := sqle.NewDefault(provider)
	wire, err := mysqlserver.NewServer(mysqlserver.Config{
		Protocol: "tcp",
		Address:  config.Address,
		Version:  "RepoDB experimental",
	}, engine, sql.NewContext, memory.NewSessionBuilder(provider), nil)
	if err != nil {
		return nil, fmt.Errorf("create MySQL server: %w", err)
	}
	return &Server{wire: wire, engine: engine, repo: config.Repository}, nil
}

func (s *Server) Address() string { return s.wire.Listener.Addr().String() }

func (s *Server) Start() error { return s.wire.Start() }

func (s *Server) Close() error {
	if err := s.wire.Close(); err != nil {
		return err
	}
	return s.engine.Close()
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
