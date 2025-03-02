// percona-everest-backend
// Copyright (C) 2023 Percona LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package api contains the API server implementation.
package api

//go:generate ../bin/oapi-codegen --config=server.cfg.yml  ../docs/spec/openapi.yml

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"sync"

	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"

	"github.com/percona/percona-everest-backend/cmd/config"
	"github.com/percona/percona-everest-backend/model"
	"github.com/percona/percona-everest-backend/pkg/kubernetes"
	"github.com/percona/percona-everest-backend/public"
)

const (
	pgStorageName   = "postgres"
	pgMigrationsDir = "migrations"
)

// EverestServer represents the server struct.
type EverestServer struct {
	config         *config.EverestConfig
	l              *zap.SugaredLogger
	storage        storage
	secretsStorage secretsStorage
	waitGroup      *sync.WaitGroup
	echo           *echo.Echo
}

// NewEverestServer creates and configures everest API.
func NewEverestServer(c *config.EverestConfig, l *zap.SugaredLogger) (*EverestServer, error) {
	e := &EverestServer{
		config:    c,
		l:         l,
		echo:      echo.New(),
		waitGroup: &sync.WaitGroup{},
	}
	if err := e.initHTTPServer(); err != nil {
		return e, err
	}
	err := e.initEverest()

	return e, err
}

func (e *EverestServer) initEverest() error {
	db, err := model.NewDatabase(pgStorageName, e.config.DSN, pgMigrationsDir)
	if err != nil {
		return err
	}
	e.storage = db
	e.secretsStorage = db // so far the db implements both interfaces - the regular storage and the secrets storage
	_, err = db.Migrate()
	return err
}

func (e *EverestServer) initKubeClient(ctx context.Context, kubernetesID string) (*model.KubernetesCluster, *kubernetes.Kubernetes, int, error) {
	k, err := e.storage.GetKubernetesCluster(ctx, kubernetesID)
	if err != nil {
		e.l.Error(err)
		return nil, nil, http.StatusBadRequest, errors.New("could not find Kubernetes cluster")
	}

	kubeClient, err := kubernetes.NewFromSecretsStorage(
		ctx, e.secretsStorage, k.ID,
		k.Namespace, e.l,
	)
	if err != nil {
		e.l.Error(err)
		return k, nil, http.StatusInternalServerError, errors.New("could not create Kubernetes client from kubeconfig")
	}

	return k, kubeClient, 0, nil
}

// initHTTPServer configures http server for the current EverestServer instance.
func (e *EverestServer) initHTTPServer() error {
	swagger, err := GetSwagger()
	if err != nil {
		return err
	}
	fsys, err := fs.Sub(public.Static, "dist")
	if err != nil {
		return errors.Join(err, errors.New("error reading filesystem"))
	}
	staticFilesHandler := http.FileServer(http.FS(fsys))
	indexFS := echo.MustSubFS(public.Index, "dist")
	// FIXME: Ideally it should be redirected to /everest/ and FE app should be served using this endpoint.
	//
	// We tried to do this with Fabio and FE app requires the following changes to be implemented:
	// 1. Add basePath configuration for react router
	// 2. Add apiUrl configuration for FE app
	//
	// Once it'll be implemented we can serve FE app on /everest/ location
	e.echo.FileFS("/*", "index.html", indexFS)
	e.echo.GET("/favicon.ico", echo.WrapHandler(staticFilesHandler))
	e.echo.GET("/assets-manifest.json", echo.WrapHandler(staticFilesHandler))
	e.echo.GET("/static/*", echo.WrapHandler(staticFilesHandler))
	// Log all requests
	e.echo.Use(echomiddleware.Logger())
	e.echo.Pre(echomiddleware.RemoveTrailingSlash())

	basePath, err := swagger.Servers.BasePath()
	if err != nil {
		return errors.Join(err, errors.New("could not get base path"))
	}

	// Use our validation middleware to check all requests against the OpenAPI schema.
	apiGroup := e.echo.Group(basePath)
	apiGroup.Use(middleware.OapiRequestValidatorWithOptions(swagger, &middleware.Options{
		SilenceServersWarning: true,
	}))
	RegisterHandlers(apiGroup, e)

	return nil
}

// Start starts everest server.
func (e *EverestServer) Start() error {
	return e.echo.Start(fmt.Sprintf("0.0.0.0:%d", e.config.HTTPPort))
}

// Shutdown gracefully stops the Everest server.
func (e *EverestServer) Shutdown(ctx context.Context) error {
	e.l.Info("Shutting down http server")
	if err := e.echo.Shutdown(ctx); err != nil {
		e.l.Error(errors.Join(err, errors.New("could not shut down http server")))
	} else {
		e.l.Info("http server shut down")
	}

	e.l.Info("Shutting down Everest")
	e.waitGroup.Wait()

	e.waitGroup.Add(1)
	go func() {
		defer e.waitGroup.Done()
		e.l.Info("Shutting down database storage")
		if err := e.storage.Close(); err != nil {
			e.l.Error(errors.Join(err, errors.New("could not shut down database storage")))
		} else {
			e.l.Info("Database storage shut down")
		}
	}()

	e.waitGroup.Add(1)
	go func() {
		defer e.waitGroup.Done()
		e.l.Info("Shutting down secrets storage")
		if err := e.secretsStorage.Close(); err != nil {
			e.l.Error(errors.Join(err, errors.New("could not shut down secret storage")))
		} else {
			e.l.Info("Secret storage shut down")
		}
	}()

	done := make(chan struct{}, 1)
	go func() {
		e.waitGroup.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *EverestServer) getBodyFromContext(ctx echo.Context, into any) error {
	// GetBody creates a copy of the body to avoid "spoiling" the request before proxing
	reader, err := ctx.Request().GetBody()
	if err != nil {
		return err
	}

	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(into); err != nil {
		return errors.Join(err, errors.New("could not decode body"))
	}
	return nil
}
