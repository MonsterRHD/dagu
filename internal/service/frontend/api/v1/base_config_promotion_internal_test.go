// Copyright (C) 2026 Yota Hamada
// SPDX-License-Identifier: GPL-3.0-or-later

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	generated "github.com/dagucloud/dagu/v2/api/v1"
	"github.com/dagucloud/dagu/v2/internal/baseconfig"
	"github.com/dagucloud/dagu/v2/internal/dagsettings"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type promoterCall struct {
	scope     baseconfig.Scope
	candidate string
}

type recordingPromoter struct {
	outcome *baseconfig.Outcome
	err     error
	calls   []promoterCall
}

func (p *recordingPromoter) Submit(_ context.Context, scope baseconfig.Scope, candidate []byte) (*baseconfig.Outcome, error) {
	p.calls = append(p.calls, promoterCall{scope: scope, candidate: string(candidate)})
	if p.err != nil {
		return nil, p.err
	}
	if p.outcome != nil {
		return p.outcome, nil
	}
	return &baseconfig.Outcome{Generation: 1}, nil
}

func newPromotionAPI(promoter *recordingPromoter) *API {
	return &API{
		baseConfigStore:    stubBaseConfigStore{},
		baseConfigPromoter: promoter,
		baseConfigProvider: func(string) (dagsettings.BaseConfigStore, error) {
			return stubBaseConfigStore{}, nil
		},
	}
}

func updateGlobalRequest(spec string) generated.UpdateBaseConfigRequestObject {
	return generated.UpdateBaseConfigRequestObject{
		Body: &generated.UpdateBaseConfigJSONRequestBody{Spec: spec},
	}
}

func TestUpdateBaseConfig_SuccessWithoutWarnings(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{}
	a := newPromotionAPI(promoter)

	response, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest("schedule: \"0 2 * * *\"\n"))
	require.NoError(t, err)
	body := response.(generated.UpdateBaseConfig200JSONResponse)
	assert.Equal(t, []string{}, body.Errors)
	require.Len(t, promoter.calls, 1)
	assert.True(t, promoter.calls[0].scope.IsGlobal())
	assert.Equal(t, "schedule: \"0 2 * * *\"\n", promoter.calls[0].candidate)
}

func TestUpdateBaseConfig_SuccessSurfacesPreExistingWarnings(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{
		outcome: &baseconfig.Outcome{
			Generation: 2,
			Warnings:   []string{`workflow "broken" (/dags/broken.yaml) already fails under the current base config: bad`},
		},
	}
	a := newPromotionAPI(promoter)

	response, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest("schedule: \"0 2 * * *\"\n"))
	require.NoError(t, err)
	body := response.(generated.UpdateBaseConfig200JSONResponse)
	require.Len(t, body.Errors, 1)
	assert.Contains(t, body.Errors[0], "broken")
	assert.Contains(t, body.Errors[0], "/dags/broken.yaml")
}

func TestUpdateBaseConfig_RegressionRejectedWithLocatableError(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{
		err: &baseconfig.RejectionError{
			Scope: baseconfig.GlobalScope(),
			Failures: []baseconfig.WorkflowFailure{{
				WorkflowRef: baseconfig.WorkflowRef{Name: "wf-a", Location: "/dags/a.yaml"},
				Reason:      "invalid schedule",
			}},
		},
	}
	a := newPromotionAPI(promoter)

	_, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest("schedule: \"0 2 * * *\"\n"))
	require.Error(t, err)
	apiErr := asAPIError(t, err)
	assert.Equal(t, http.StatusBadRequest, apiErr.HTTPStatus)
	assert.Contains(t, apiErr.Message, "wf-a")
	assert.Contains(t, apiErr.Message, "/dags/a.yaml")
	assert.Contains(t, apiErr.Message, "invalid schedule")
	require.Len(t, promoter.calls, 1)
}

func TestUpdateBaseConfig_SyntaxErrorShortCircuitsBeforeCandidate(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{}
	a := newPromotionAPI(promoter)

	_, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest("schedule: ["))
	require.Error(t, err)
	apiErr := asAPIError(t, err)
	assert.Equal(t, http.StatusBadRequest, apiErr.HTTPStatus)
	assert.Empty(t, promoter.calls)
}

func TestUpdateBaseConfig_EmptySpecStillReachesPromoter(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{}
	a := newPromotionAPI(promoter)

	response, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest(""))
	require.NoError(t, err)
	assert.Equal(t, []string{}, response.(generated.UpdateBaseConfig200JSONResponse).Errors)
	require.Len(t, promoter.calls, 1)
	assert.Equal(t, "", promoter.calls[0].candidate)
}

func TestUpdateBaseConfig_PublishFailureReturns500(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{err: fmt.Errorf("%w: disk full", baseconfig.ErrPublish)}
	a := newPromotionAPI(promoter)

	_, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest("schedule: \"0 2 * * *\"\n"))
	require.Error(t, err)
	assert.Same(t, ErrFailedToSaveBaseConfig, err)
}

func TestUpdateBaseConfig_ValidationInfrastructureFailureReturns500(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{err: errors.New("enumerator unavailable")}
	a := newPromotionAPI(promoter)

	_, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest("schedule: \"0 2 * * *\"\n"))
	require.Error(t, err)
	assert.Same(t, ErrFailedToValidateBaseConfig, err)
}

func TestUpdateBaseConfig_StaleFenceReturns400(t *testing.T) {
	t.Parallel()
	promoter := &recordingPromoter{err: baseconfig.ErrFence}
	a := newPromotionAPI(promoter)

	_, err := a.UpdateBaseConfig(context.Background(), updateGlobalRequest("schedule: \"0 2 * * *\"\n"))
	require.Error(t, err)
	apiErr := asAPIError(t, err)
	assert.Equal(t, http.StatusBadRequest, apiErr.HTTPStatus)
}

func asAPIError(t *testing.T, err error) *Error {
	t.Helper()
	var apiErr *Error
	require.True(t, errors.As(err, &apiErr))
	return apiErr
}
