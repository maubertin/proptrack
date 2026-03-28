package db

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dgraph-io/dgo/v2"
	"github.com/dgraph-io/dgo/v2/protos/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	client     *dgo.Dgraph
	clientOnce sync.Once
	schemaOnce sync.Once
)

// Client returns the singleton Dgraph client, initializing it on first call.
func Client() *dgo.Dgraph {
	clientOnce.Do(func() {
		host := os.Getenv("DGRAPH_ALPHA_HOST")
		if host == "" {
			host = "localhost"
		}
		portStr := os.Getenv("DGRAPH_ALPHA_PORT")
		if portStr == "" {
			portStr = "9080"
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			slog.Error("invalid DGRAPH_ALPHA_PORT", "err", err)
			os.Exit(1)
		}

		addr := fmt.Sprintf("%s:%d", host, port)
		slog.Info("connecting to Dgraph", "addr", addr)

		var conn *grpc.ClientConn
		for attempt := 1; attempt <= 20; attempt++ {
			conn, err = grpc.Dial(addr,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithBlock(),
				grpc.WithTimeout(3*time.Second),
			)
			if err == nil {
				break
			}
			slog.Warn("dgraph not ready, retrying", "attempt", attempt, "err", err)
			time.Sleep(3 * time.Second)
		}
		if err != nil {
			slog.Error("failed to connect to Dgraph after retries", "err", err)
			os.Exit(1)
		}

		client = dgo.NewDgraphClient(api.NewDgraphClient(conn))
		slog.Info("connected to Dgraph", "addr", addr)
	})
	return client
}

// SetupSchema applies the DQL schema to Dgraph exactly once per process.
func SetupSchema() error {
	var outerErr error
	schemaOnce.Do(func() {
		c := Client()
		op := &api.Operation{
			Schema: DQLSchema,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := c.Alter(ctx, op); err != nil {
			outerErr = fmt.Errorf("schema setup failed: %w", err)
			return
		}
		slog.Info("Dgraph schema applied successfully")
	})
	return outerErr
}

// NewTxn returns a new read-write transaction.
func NewTxn() *dgo.Txn {
	return Client().NewTxn()
}

// NewReadOnlyTxn returns a new read-only transaction.
func NewReadOnlyTxn() *dgo.Txn {
	return Client().NewReadOnlyTxn()
}
