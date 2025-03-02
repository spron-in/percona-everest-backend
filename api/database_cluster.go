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

// Package api ...
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/AlekSi/pointer"
	"github.com/labstack/echo/v4"
	everestv1alpha1 "github.com/percona/everest-operator/api/v1alpha1"

	"github.com/percona/percona-everest-backend/pkg/kubernetes"
)

// CreateDatabaseCluster creates a new db cluster inside the given k8s cluster.
func (e *EverestServer) CreateDatabaseCluster(ctx echo.Context, kubernetesID string) error {
	dbc := &DatabaseCluster{}
	if err := e.getBodyFromContext(ctx, dbc); err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString("Could not get DatabaseCluster from the request body"),
		})
	}

	if err := e.validateDatabaseClusterCR(ctx, kubernetesID, dbc); err != nil {
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString(err.Error())})
	}

	_, kubeClient, code, err := e.initKubeClient(ctx.Request().Context(), kubernetesID)
	if err != nil {
		return ctx.JSON(code, Error{Message: pointer.ToString(err.Error())})
	}

	backupNames := backupStorageNamesFrom(dbc)
	err = e.createK8SBackupStorages(ctx.Request().Context(), kubeClient, backupNames)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Could not create BackupStorage"),
		})
	}

	if monitoringName := monitoringNameFrom(dbc); monitoringName != "" {
		i, err := e.storage.GetMonitoringInstance(monitoringName)
		if err != nil {
			return ctx.JSON(http.StatusBadRequest, Error{
				Message: pointer.ToString("Could not find monitoring instance"),
			})
		}

		err = kubeClient.EnsureConfigExists(ctx.Request().Context(), i, e.secretsStorage.GetSecret)
		if err != nil {
			e.l.Error(err)
			return ctx.JSON(http.StatusBadRequest, Error{
				Message: pointer.ToString("Could not create monitoring config in Kubernetes"),
			})
		}
	}

	return e.proxyKubernetes(ctx, kubernetesID, "")
}

// ListDatabaseClusters lists the created database clusters on the specified kubernetes cluster.
func (e *EverestServer) ListDatabaseClusters(ctx echo.Context, kubernetesID string) error {
	return e.proxyKubernetes(ctx, kubernetesID, "")
}

// DeleteDatabaseCluster deletes a database cluster on the specified kubernetes cluster.
func (e *EverestServer) DeleteDatabaseCluster(ctx echo.Context, kubernetesID string, name string) error {
	_, kubeClient, code, err := e.initKubeClient(ctx.Request().Context(), kubernetesID)
	if err != nil {
		return ctx.JSON(code, Error{Message: pointer.ToString(err.Error())})
	}

	db, err := kubeClient.GetDatabaseCluster(ctx.Request().Context(), name)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{
			Message: pointer.ToString("Could not get database cluster"),
		})
	}

	proxyErr := e.proxyKubernetes(ctx, kubernetesID, name)
	if proxyErr != nil {
		return proxyErr
	}

	// At this point the proxy already sent a response to the API user.
	// We check if the response was successful to continue with cleanup.
	if ctx.Response().Status >= http.StatusMultipleChoices {
		return nil
	}

	names := kubernetes.BackupStorageNamesFromDBCluster(db)
	e.waitGroup.Add(1)
	go e.deleteK8SBackupStorages(context.Background(), kubeClient, names)

	if db.Spec.Monitoring != nil && db.Spec.Monitoring.MonitoringConfigName != "" {
		e.waitGroup.Add(1)
		go e.deleteK8SMonitoringConfig(context.Background(), kubeClient, db.Spec.Monitoring.MonitoringConfigName)
	}

	return nil
}

// GetDatabaseCluster retrieves the specified database cluster on the specified kubernetes cluster.
func (e *EverestServer) GetDatabaseCluster(ctx echo.Context, kubernetesID string, name string) error {
	return e.proxyKubernetes(ctx, kubernetesID, name)
}

// UpdateDatabaseCluster replaces the specified database cluster on the specified kubernetes cluster.
func (e *EverestServer) UpdateDatabaseCluster(ctx echo.Context, kubernetesID string, name string) error {
	dbc := &DatabaseCluster{}
	if err := e.getBodyFromContext(ctx, dbc); err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusBadRequest, Error{
			Message: pointer.ToString("Could not get DatabaseCluster from the request body"),
		})
	}

	if err := e.validateDatabaseClusterCR(ctx, kubernetesID, dbc); err != nil {
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString(err.Error())})
	}

	_, kubeClient, code, err := e.initKubeClient(ctx.Request().Context(), kubernetesID)
	if err != nil {
		return ctx.JSON(code, Error{Message: pointer.ToString(err.Error())})
	}

	oldDB, err := kubeClient.GetDatabaseCluster(ctx.Request().Context(), name)
	if err != nil {
		return errors.Join(err, errors.New("could not get old Database Cluster"))
	}
	if err := validateDatabaseClusterOnUpdate(dbc, oldDB); err != nil {
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString(err.Error())})
	}
	newMonitoringName := monitoringNameFrom(dbc)
	newBackupNames := backupStorageNamesFrom(dbc)
	err = e.createResources(ctx.Request().Context(), oldDB, kubeClient, newMonitoringName, newBackupNames)
	if err != nil {
		return ctx.JSON(http.StatusInternalServerError, Error{Message: pointer.ToString(err.Error())})
	}

	proxyErr := e.proxyKubernetes(ctx, kubernetesID, name)
	if proxyErr != nil {
		return proxyErr
	}

	// At this point the proxy already sent a response to the API user.
	// We check if the response was successful to continue with cleanup.
	if ctx.Response().Status >= http.StatusMultipleChoices {
		return nil
	}
	e.waitGroup.Add(1)
	go e.deleteBackupStoragesOnUpdate(context.Background(), kubeClient, oldDB, newBackupNames)
	e.waitGroup.Add(1)
	go e.deleteMonitoringInstanceOnUpdate(context.Background(), kubeClient, oldDB, newMonitoringName)

	return nil
}

func (e *EverestServer) createResources(c context.Context, oldDB *everestv1alpha1.DatabaseCluster, kubeClient *kubernetes.Kubernetes, newMonitoringName string, newBackupNames map[string]struct{}) error {
	oldNames := withBackupStorageNamesFromDBCluster(make(map[string]struct{}), *oldDB)
	err := e.createBackupStoragesOnUpdate(c, kubeClient, oldNames, newBackupNames)
	if err != nil {
		e.l.Error(err)
		return errors.New("could not create new BackupStorages in Kubernetes")
	}

	err = e.createMonitoringInstanceOnUpdate(c, kubeClient, oldDB, newMonitoringName)
	if err != nil {
		e.l.Error(err)
		return errors.New("could not create new monitoring configs in Kubernetes")
	}
	return nil
}

// GetDatabaseClusterCredentials returns credentials for the specified database cluster on the specified kubernetes cluster.
func (e *EverestServer) GetDatabaseClusterCredentials(ctx echo.Context, kubernetesID string, name string) error {
	k, kubeClient, code, err := e.initKubeClient(ctx.Request().Context(), kubernetesID)
	if err != nil {
		return ctx.JSON(code, Error{Message: pointer.ToString(err.Error())})
	}

	databaseCluster, err := kubeClient.GetDatabaseCluster(ctx.Request().Context(), name)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{Message: pointer.ToString(err.Error())})
	}
	secret, err := kubeClient.GetSecret(ctx.Request().Context(), databaseCluster.Spec.Engine.UserSecretsName, k.Namespace)
	if err != nil {
		e.l.Error(err)
		return ctx.JSON(http.StatusInternalServerError, Error{Message: pointer.ToString(err.Error())})
	}
	response := &DatabaseClusterCredential{}
	switch databaseCluster.Spec.Engine.Type {
	case everestv1alpha1.DatabaseEnginePXC:
		response.Username = pointer.ToString("root")
		response.Password = pointer.ToString(string(secret.Data["root"]))
	case everestv1alpha1.DatabaseEnginePSMDB:
		response.Username = pointer.ToString(string(secret.Data["MONGODB_USER_ADMIN_USER"]))
		response.Password = pointer.ToString(string(secret.Data["MONGODB_USER_ADMIN_PASSWORD"]))
	case everestv1alpha1.DatabaseEnginePostgresql:
		response.Username = pointer.ToString("postgres")
		response.Password = pointer.ToString(string(secret.Data["password"]))
	default:
		return ctx.JSON(http.StatusBadRequest, Error{Message: pointer.ToString("Unsupported database engine")})
	}

	return ctx.JSON(http.StatusOK, response)
}

func (e *EverestServer) createK8SBackupStorages(ctx context.Context, kubeClient *kubernetes.Kubernetes, names map[string]struct{}) error {
	if len(names) == 0 {
		return nil
	}

	processed := make([]string, 0, len(names))
	for name := range names {
		bs, err := e.storage.GetBackupStorage(ctx, nil, name)
		if err != nil {
			return errors.Join(err, errors.New("could not get backup storage"))
		}

		err = kubeClient.EnsureConfigExists(ctx, bs, e.secretsStorage.GetSecret)
		if err != nil {
			e.rollbackCreatedBackupStorages(ctx, kubeClient, processed)
			return errors.Join(err, fmt.Errorf("could not create CRs for %s", name))
		}
		processed = append(processed, name)
	}
	return nil
}

func (e *EverestServer) rollbackCreatedBackupStorages(ctx context.Context, kubeClient *kubernetes.Kubernetes, toDelete []string) {
	for _, name := range toDelete {
		bs, err := e.storage.GetBackupStorage(ctx, nil, name)
		if err != nil {
			e.l.Error(errors.Join(err, errors.New("could not get backup storage")))
			continue
		}

		err = kubeClient.DeleteConfig(ctx, bs, func(ctx context.Context, name string) (bool, error) {
			return kubernetes.IsBackupStorageConfigInUse(ctx, name, kubeClient)
		})
		if err != nil && !errors.Is(err, kubernetes.ErrConfigInUse) {
			e.l.Error(errors.Join(err, errors.New("could not delete backup storage config")))
			continue
		}
	}
}

func (e *EverestServer) deleteK8SMonitoringConfig(
	ctx context.Context, kubeClient *kubernetes.Kubernetes, name string,
) {
	defer e.waitGroup.Done()
	i, err := e.storage.GetMonitoringInstance(name)
	if err != nil {
		e.l.Error(errors.Join(err, errors.New("could get monitoring instance")))
		return
	}

	err = kubeClient.DeleteConfig(ctx, i, func(ctx context.Context, name string) (bool, error) {
		return kubernetes.IsMonitoringConfigInUse(ctx, name, kubeClient)
	})
	if err != nil && !errors.Is(err, kubernetes.ErrConfigInUse) {
		e.l.Error(errors.Join(err, errors.New("could not delete monitoring config in Kubernetes")))
		return
	}
}

func (e *EverestServer) deleteK8SBackupStorages(
	ctx context.Context, kubeClient *kubernetes.Kubernetes, names map[string]struct{},
) {
	defer e.waitGroup.Done()
	for name := range names {
		bs, err := e.storage.GetBackupStorage(ctx, nil, name)
		if err != nil {
			e.l.Error(errors.Join(err, errors.New("could not get backup storage")))
			continue
		}

		err = kubeClient.DeleteConfig(ctx, bs, func(ctx context.Context, name string) (bool, error) {
			return kubernetes.IsBackupStorageConfigInUse(ctx, name, kubeClient)
		})
		if err != nil && !errors.Is(err, kubernetes.ErrConfigInUse) {
			e.l.Error(errors.Join(err, errors.New("could not delete backup storage config in Kubernetes")))
			continue
		}
	}
}

func (e *EverestServer) deleteK8SBackupStorage(
	ctx context.Context, kubeClient *kubernetes.Kubernetes, name string,
) error {
	bs, err := e.storage.GetBackupStorage(ctx, nil, name)
	if err != nil {
		return errors.Join(err, errors.New("could not find backup storage"))
	}

	err = kubeClient.DeleteConfig(ctx, bs, func(ctx context.Context, name string) (bool, error) {
		return kubernetes.IsBackupStorageConfigInUse(ctx, name, kubeClient)
	})
	if err != nil && !errors.Is(err, kubernetes.ErrConfigInUse) {
		return errors.Join(err, errors.New("could not delete config in Kubernetes"))
	}

	return nil
}

func (e *EverestServer) createBackupStoragesOnUpdate(
	ctx context.Context,
	kubeClient *kubernetes.Kubernetes,
	oldNames map[string]struct{},
	newNames map[string]struct{},
) error {
	// try to create all storages that are new
	toCreate := uniqueKeys(oldNames, newNames)
	processed := make([]string, 0, len(toCreate))
	for name := range toCreate {
		bs, err := e.storage.GetBackupStorage(ctx, nil, name)
		if err != nil {
			return errors.Join(err, errors.New("could not get backup storage"))
		}

		err = kubeClient.EnsureConfigExists(ctx, bs, e.secretsStorage.GetSecret)
		if err != nil {
			e.rollbackCreatedBackupStorages(ctx, kubeClient, processed)
			return errors.Join(err, fmt.Errorf("could not create CRs for %s", name))
		}
		processed = append(processed, name)
	}

	return nil
}

func (e *EverestServer) deleteBackupStoragesOnUpdate(
	ctx context.Context,
	kubeClient *kubernetes.Kubernetes,
	oldDB *everestv1alpha1.DatabaseCluster,
	newNames map[string]struct{},
) {
	defer e.waitGroup.Done()
	oldNames := withBackupStorageNamesFromDBCluster(make(map[string]struct{}), *oldDB)
	toDelete := uniqueKeys(newNames, oldNames)
	for name := range toDelete {
		err := e.deleteK8SBackupStorage(ctx, kubeClient, name)
		if err != nil && !errors.Is(err, kubernetes.ErrConfigInUse) {
			e.l.Error(errors.Join(err, fmt.Errorf("could not delete CRs for %s", name)))
		}
	}
}

func (e *EverestServer) createMonitoringInstanceOnUpdate(
	ctx context.Context,
	kubeClient *kubernetes.Kubernetes,
	oldDB *everestv1alpha1.DatabaseCluster,
	newName string,
) error {
	oldName := ""
	if oldDB.Spec.Monitoring != nil {
		oldName = oldDB.Spec.Monitoring.MonitoringConfigName
	}

	if newName != "" && newName != oldName {
		i, err := e.storage.GetMonitoringInstance(newName)
		if err != nil {
			return errors.Join(err, errors.New("could not get monitoring instance"))
		}

		err = kubeClient.EnsureConfigExists(ctx, i, e.secretsStorage.GetSecret)
		if err != nil {
			return errors.Join(err, errors.New("could not create monitoring config in Kubernetes"))
		}
	}

	return nil
}

func (e *EverestServer) deleteMonitoringInstanceOnUpdate(
	ctx context.Context,
	kubeClient *kubernetes.Kubernetes,
	oldDB *everestv1alpha1.DatabaseCluster,
	newName string,
) {
	defer e.waitGroup.Done()
	oldName := ""
	if oldDB.Spec.Monitoring != nil {
		oldName = oldDB.Spec.Monitoring.MonitoringConfigName
	}

	if oldName != "" && newName != oldName {
		i, err := e.storage.GetMonitoringInstance(oldName)
		if err != nil {
			e.l.Error(errors.Join(err, errors.New("could not get monitoring instance")))
			return
		}

		err = kubeClient.DeleteConfig(ctx, i, func(ctx context.Context, name string) (bool, error) {
			return kubernetes.IsMonitoringConfigInUse(ctx, name, kubeClient)
		})
		if err != nil && !errors.Is(err, kubernetes.ErrConfigInUse) {
			e.l.Error(errors.Join(err, errors.New("could not delete monitoring config from Kubernetes")))
			return
		}
	}
}

func backupStorageNamesFrom(dbc *DatabaseCluster) map[string]struct{} {
	names := make(map[string]struct{})
	if dbc.Spec == nil {
		return names
	}

	if dbc.Spec.DataSource != nil && dbc.Spec.DataSource.BackupSource != nil {
		names[dbc.Spec.DataSource.BackupSource.BackupStorageName] = struct{}{}
	}

	if dbc.Spec.Backup == nil || dbc.Spec.Backup.Schedules == nil {
		return names
	}
	for _, schedule := range *dbc.Spec.Backup.Schedules {
		names[schedule.BackupStorageName] = struct{}{}
	}

	return names
}

func monitoringNameFrom(db *DatabaseCluster) string {
	if db.Spec == nil {
		return ""
	}

	if db.Spec.Monitoring == nil {
		return ""
	}
	if db.Spec.Monitoring.MonitoringConfigName == nil {
		return ""
	}

	return *db.Spec.Monitoring.MonitoringConfigName
}

func withBackupStorageNamesFromDBCluster(existing map[string]struct{}, dbc everestv1alpha1.DatabaseCluster) map[string]struct{} {
	if dbc.Spec.DataSource != nil && dbc.Spec.DataSource.BackupSource != nil && dbc.Spec.DataSource.BackupSource.BackupStorageName != "" {
		existing[dbc.Spec.DataSource.BackupSource.BackupStorageName] = struct{}{}
	}

	for _, schedule := range dbc.Spec.Backup.Schedules {
		if schedule.BackupStorageName != "" {
			existing[schedule.BackupStorageName] = struct{}{}
		}
	}

	return existing
}

func uniqueKeys(source, target map[string]struct{}) map[string]struct{} {
	keysNotInSource := make(map[string]struct{}, len(target))
	for key := range target {
		if _, exists := source[key]; !exists {
			keysNotInSource[key] = struct{}{}
		}
	}
	return keysNotInSource
}
