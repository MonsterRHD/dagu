// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/dagucloud/dagu/v2/api/v1"
	"github.com/dagucloud/dagu/v2/internal/audit"
	"github.com/dagucloud/dagu/v2/internal/baseconfig"
	"github.com/dagucloud/dagu/v2/internal/cmn/logger"
	"github.com/dagucloud/dagu/v2/internal/cmn/logger/tag"
	"github.com/dagucloud/dagu/v2/internal/dagsettings"
	"github.com/dagucloud/dagu/v2/internal/ir"
	"github.com/dagucloud/dagu/v2/internal/spec"
	"github.com/dagucloud/dagu/v2/internal/workspace"
)

var (
	// ErrBaseConfigNotAvailable is returned when base config management is not configured.
	ErrBaseConfigNotAvailable = &Error{
		Code:       api.ErrorCodeForbidden,
		Message:    "Base configuration management is not available",
		HTTPStatus: http.StatusForbidden,
	}

	// ErrFailedToLoadBaseConfig is returned when reading the base config fails.
	ErrFailedToLoadBaseConfig = &Error{
		Code:       api.ErrorCodeInternalError,
		Message:    "Failed to load base configuration",
		HTTPStatus: http.StatusInternalServerError,
	}

	// ErrFailedToSaveBaseConfig is returned when writing the base config fails.
	ErrFailedToSaveBaseConfig = &Error{
		Code:       api.ErrorCodeInternalError,
		Message:    "Failed to save base configuration",
		HTTPStatus: http.StatusInternalServerError,
	}

	// ErrFailedToLoadWorkspaceBaseConfig is returned when reading workspace config fails.
	ErrFailedToLoadWorkspaceBaseConfig = &Error{
		Code:       api.ErrorCodeInternalError,
		Message:    "Failed to load workspace base configuration",
		HTTPStatus: http.StatusInternalServerError,
	}

	// ErrFailedToSaveWorkspaceBaseConfig is returned when writing workspace config fails.
	ErrFailedToSaveWorkspaceBaseConfig = &Error{
		Code:       api.ErrorCodeInternalError,
		Message:    "Failed to save workspace base configuration",
		HTTPStatus: http.StatusInternalServerError,
	}

	// ErrFailedToValidateBaseConfig is returned when rebuilding workflows
	// against a candidate fails for an infrastructure reason instead of a
	// workflow regression. The live configuration stays unchanged.
	ErrFailedToValidateBaseConfig = &Error{
		Code:       api.ErrorCodeInternalError,
		Message:    "Failed to validate base configuration against workflows",
		HTTPStatus: http.StatusInternalServerError,
	}
	ErrFailedToValidateWorkspaceBaseConfig = &Error{
		Code:       api.ErrorCodeInternalError,
		Message:    "Failed to validate workspace base configuration against workflows",
		HTTPStatus: http.StatusInternalServerError,
	}
)

// GetBaseConfig returns the current base DAG configuration YAML. Requires developer role or above.
func (a *API) GetBaseConfig(ctx context.Context, _ api.GetBaseConfigRequestObject) (api.GetBaseConfigResponseObject, error) {
	if err := a.requireBaseConfigManagement(); err != nil {
		return nil, err
	}
	if err := a.requireDeveloperOrAbove(ctx); err != nil {
		return nil, err
	}

	yamlSpec, err := a.baseConfigStore.GetSpec(ctx)
	if err != nil {
		logger.Error(ctx, "Failed to load base config", tag.Error(err))
		return nil, ErrFailedToLoadBaseConfig
	}

	errs := validateBaseConfig(ctx, yamlSpec)
	if errs == nil {
		errs = []string{}
	}

	return api.GetBaseConfig200JSONResponse{
		Spec:   yamlSpec,
		Errors: errs,
	}, nil
}

// UpdateBaseConfig validates and saves the base DAG configuration YAML. Requires developer role or above.
func (a *API) UpdateBaseConfig(ctx context.Context, request api.UpdateBaseConfigRequestObject) (api.UpdateBaseConfigResponseObject, error) {
	if err := a.requireBaseConfigManagement(); err != nil {
		return nil, err
	}
	if err := a.requireDeveloperOrAbove(ctx); err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, ErrInvalidRequestBody
	}

	// Validate standalone syntax before forming a candidate generation.
	if errs := validateBaseConfig(ctx, request.Body.Spec); len(errs) > 0 {
		return nil, &Error{
			Code:       api.ErrorCodeBadRequest,
			Message:    strings.Join(errs, "; "),
			HTTPStatus: http.StatusBadRequest,
		}
	}

	outcome, err := a.baseConfigPromoter.Submit(ctx, baseconfig.GlobalScope(), []byte(request.Body.Spec))
	if err != nil {
		return nil, a.mapBaseConfigPromotionError(ctx, err, "global",
			ErrFailedToSaveBaseConfig, ErrFailedToValidateBaseConfig)
	}

	a.logAudit(ctx, audit.CategorySystem, "base_config_update", nil)

	return api.UpdateBaseConfig200JSONResponse{
		Errors: promotionWarnings(outcome),
	}, nil
}

// GetWorkspaceBaseConfig returns the workspace-scoped base DAG configuration YAML.
func (a *API) GetWorkspaceBaseConfig(
	ctx context.Context,
	request api.GetWorkspaceBaseConfigRequestObject,
) (api.GetWorkspaceBaseConfigResponseObject, error) {
	workspaceName, err := a.resolveWorkspaceBaseConfigTarget(ctx, request.WorkspaceName)
	if err != nil {
		return nil, err
	}
	if err := a.requireWorkspaceVisible(ctx, workspaceName); err != nil {
		return nil, err
	}

	store, err := a.workspaceBaseConfigStore(workspaceName)
	if err != nil {
		logger.Error(ctx, "Failed to initialize workspace base config store", tag.Name(workspaceName), tag.Error(err))
		return nil, ErrFailedToLoadWorkspaceBaseConfig
	}
	yamlSpec, err := store.GetSpec(ctx)
	if err != nil {
		logger.Error(ctx, "Failed to load workspace base config", tag.Name(workspaceName), tag.Error(err))
		return nil, ErrFailedToLoadWorkspaceBaseConfig
	}

	errs := validateBaseConfig(ctx, yamlSpec)
	if errs == nil {
		errs = []string{}
	}

	return api.GetWorkspaceBaseConfig200JSONResponse{
		Spec:   yamlSpec,
		Errors: errs,
	}, nil
}

// UpdateWorkspaceBaseConfig validates and saves a workspace-scoped base DAG configuration YAML.
func (a *API) UpdateWorkspaceBaseConfig(
	ctx context.Context,
	request api.UpdateWorkspaceBaseConfigRequestObject,
) (api.UpdateWorkspaceBaseConfigResponseObject, error) {
	workspaceName, err := a.resolveWorkspaceBaseConfigTarget(ctx, request.WorkspaceName)
	if err != nil {
		return nil, err
	}
	if err := a.requireWorkspaceConfigWrite(ctx, workspaceName); err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, ErrInvalidRequestBody
	}

	if errs := validateBaseConfig(ctx, request.Body.Spec); len(errs) > 0 {
		return nil, &Error{
			Code:       api.ErrorCodeBadRequest,
			Message:    strings.Join(errs, "; "),
			HTTPStatus: http.StatusBadRequest,
		}
	}

	scope, err := baseconfig.WorkspaceScope(workspaceName)
	if err != nil {
		return nil, &Error{
			Code:       api.ErrorCodeBadRequest,
			Message:    err.Error(),
			HTTPStatus: http.StatusBadRequest,
		}
	}
	outcome, err := a.baseConfigPromoter.Submit(ctx, scope, []byte(request.Body.Spec))
	if err != nil {
		return nil, a.mapBaseConfigPromotionError(ctx, err, workspaceName,
			ErrFailedToSaveWorkspaceBaseConfig, ErrFailedToValidateWorkspaceBaseConfig)
	}

	a.logAudit(ctx, audit.CategoryWorkspace, "workspace_base_config_update", map[string]string{
		"workspace": workspaceName,
	})

	return api.UpdateWorkspaceBaseConfig200JSONResponse{
		Errors: promotionWarnings(outcome),
	}, nil
}

// mapBaseConfigPromotionError translates promotion failures into API errors.
// Workflow regressions and generation fencing stay client errors (400) with
// workflow-locatable messages; publication and infrastructure failures stay
// server errors (500) so callers know the previous generation is still live.
func (a *API) mapBaseConfigPromotionError(
	ctx context.Context,
	err error,
	scopeLabel string,
	publishErr *Error,
	validationErr *Error,
) *Error {
	if rejected, ok := baseconfig.AsRejection(err); ok {
		return &Error{
			Code:       api.ErrorCodeBadRequest,
			Message:    rejected.Error(),
			HTTPStatus: http.StatusBadRequest,
		}
	}
	if errors.Is(err, baseconfig.ErrFence) {
		return &Error{
			Code:       api.ErrorCodeBadRequest,
			Message:    err.Error(),
			HTTPStatus: http.StatusBadRequest,
		}
	}
	logger.Error(ctx, "Failed to promote base config", tag.Name(scopeLabel), tag.Error(err))
	if errors.Is(err, baseconfig.ErrPublish) {
		return publishErr
	}
	return validationErr
}

func promotionWarnings(outcome *baseconfig.Outcome) []string {
	if outcome == nil || len(outcome.Warnings) == 0 {
		return []string{}
	}
	return append([]string(nil), outcome.Warnings...)
}

func (a *API) requireBaseConfigManagement() error {
	if a.baseConfigStore == nil || a.baseConfigProvider == nil || a.baseConfigPromoter == nil {
		return ErrBaseConfigNotAvailable
	}
	return nil
}

func (a *API) resolveWorkspaceBaseConfigTarget(ctx context.Context, name string) (string, error) {
	if err := a.requireBaseConfigManagement(); err != nil {
		return "", err
	}
	if a.workspaceStore == nil {
		return "", workspaceStoreUnavailable()
	}
	workspaceName, err := validateWorkspaceParam(name)
	if err != nil {
		return "", err
	}
	if workspaceName == "" {
		return "", badWorkspaceError("workspace name is required")
	}

	ws, err := a.workspaceStore.GetByName(ctx, workspaceName)
	if err != nil {
		if errors.Is(err, workspace.ErrWorkspaceNotFound) {
			return "", workspaceResourceNotFound()
		}
		return "", err
	}
	return ws.Name, nil
}

func (a *API) requireWorkspaceConfigWrite(ctx context.Context, workspaceName string) error {
	role, ok, err := a.effectiveRoleForWorkspace(ctx, workspaceName)
	if err != nil {
		return err
	}
	if !ok {
		return workspaceResourceNotFound()
	}
	if !role.CanWrite() {
		return errInsufficientPermissions
	}
	return nil
}

func (a *API) workspaceBaseConfigStore(workspaceName string) (dagsettings.BaseConfigStore, error) {
	if a.baseConfigProvider == nil {
		return nil, errors.New("workspace base config provider is not configured")
	}
	return a.baseConfigProvider(workspaceName)
}

// validateBaseConfig parses the YAML spec and returns any validation errors.
// Returns nil when the spec is empty or valid.
func validateBaseConfig(ctx context.Context, yamlSpec string) []string {
	if yamlSpec == "" {
		return nil
	}

	_, err := spec.LoadYAML(ctx,
		[]byte(yamlSpec),
		spec.WithoutEval(),
	)

	if loadErrs, ok := errors.AsType[ir.ErrorList](err); ok {
		return loadErrs.ToStringList()
	}
	if err != nil {
		return []string{err.Error()}
	}

	return nil
}
