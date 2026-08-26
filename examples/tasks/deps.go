package main

// The objects built once at boot and needed by many handlers: a logger, a
// service client, a store. They live in one typed holder, so handlers keep the
// standard net/http signature and still reach them without an assertion.

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/vieolo/vov"
)

// deps is the application's single dependency holder. Handlers reach it with
// deps.Get(), which is typed — no assertion, and a wrong field is a compile
// error. main populates it once, and lists it in AppConfig.RequireDeps so that
// forgetting to is a construction error rather than a panic in production.
var deps = vov.NewDependencies[*Deps]()

// Deps holds what the handlers share. vov never looks inside it.
type Deps struct {
	Log   *slog.Logger
	S3    *s3Client
	Store *taskStore // stands in for the database
}

func newDeps(cfg config) *Deps {
	level := slog.LevelInfo
	if cfg.Debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	return &Deps{
		Log:   log,
		S3:    &s3Client{bucket: cfg.Bucket, log: log},
		Store: newTaskStore(cfg.MaxTasks),
	}
}

// s3Client stands in for an AWS SDK client: built once, shared by every handler
// that needs it, and never constructed per request.
type s3Client struct {
	bucket string
	log    *slog.Logger
}

// PutObject pretends to upload. A real client would return an error worth
// handling; this one only records that it was reached.
func (c *s3Client) PutObject(key string, body []byte) (string, error) {
	url := fmt.Sprintf("s3://%s/%s", c.bucket, key)
	c.log.Info("archived", "url", url, "bytes", len(body))
	return url, nil
}
