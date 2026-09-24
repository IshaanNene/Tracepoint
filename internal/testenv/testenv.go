//go:build integration || e2e

// Package testenv starts real datastores in containers for the integration and
// known-answer suites. It is compiled only under those build tags, so the default test
// run and the binary never depend on Docker.
package testenv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Images are pinned by tag so a suite run is reproducible.
const (
	PostgresImage = "postgres:16-alpine"
	RedisImage    = "redis:7-alpine"
	MySQLImage    = "mysql:8.4"
)

func start(t testing.TB, req testcontainers.ContainerRequest, port string) (host string, mapped int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req, Started: true,
	})
	testcontainers.CleanupContainer(t, c)
	if err != nil {
		t.Fatalf("starting %s: %v", req.Image, err)
	}
	h, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	p, err := c.MappedPort(ctx, port)
	if err != nil {
		t.Fatalf("container port: %v", err)
	}
	return h, int(p.Num())
}

// Postgres starts a server and returns a DSN for database app as a superuser. It
// allows 500 connections, because a table lock under load holds one per blocked
// request and the default of 100 would turn the fault into connection refusals.
func Postgres(t testing.TB) string {
	t.Helper()
	host, port := start(t, testcontainers.ContainerRequest{
		Image:        PostgresImage,
		Env:          map[string]string{"POSTGRES_USER": "tp", "POSTGRES_PASSWORD": "tp", "POSTGRES_DB": "app"},
		Cmd:          []string{"-c", "max_connections=500"},
		ExposedPorts: []string{"5432/tcp"},
		WaitingFor: wait.ForAll(
			// The server restarts once after initialisation, so the line appears twice.
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			wait.ForListeningPort("5432/tcp"),
		).WithDeadline(3 * time.Minute),
	}, "5432/tcp")
	return fmt.Sprintf("postgres://tp:tp@%s:%d/app?sslmode=disable", host, port)
}

// Redis starts a server and returns its address. debug enables DEBUG SLEEP, which the
// known-answer suite uses to freeze the server at a known time.
func Redis(t testing.TB, debug bool) string {
	t.Helper()
	cmd := []string{"redis-server"}
	if debug {
		cmd = append(cmd, "--enable-debug-command", "yes")
	}
	host, port := start(t, testcontainers.ContainerRequest{
		Image:        RedisImage,
		Cmd:          cmd,
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp").WithStartupTimeout(time.Minute),
	}, "6379/tcp")
	return fmt.Sprintf("%s:%d", host, port)
}

// MySQL starts a server and returns a DSN for database app as root.
func MySQL(t testing.TB) string {
	t.Helper()
	host, port := start(t, testcontainers.ContainerRequest{
		Image:        MySQLImage,
		Env:          map[string]string{"MYSQL_ROOT_PASSWORD": "tp", "MYSQL_DATABASE": "app"},
		ExposedPorts: []string{"3306/tcp"},
		WaitingFor: wait.ForAll(
			wait.ForLog("ready for connections").WithOccurrence(2),
			wait.ForListeningPort("3306/tcp"),
		).WithDeadline(4 * time.Minute),
	}, "3306/tcp")
	return fmt.Sprintf("root:tp@tcp(%s:%d)/app", host, port)
}
