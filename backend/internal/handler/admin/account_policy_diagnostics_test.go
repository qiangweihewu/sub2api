package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGetUpstreamPolicyDiagnosticsRejectsInvalidAccountAndGroupIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newPolicyDiagnosticsHandler(t, &service.Account{ID: 8, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, GroupIDs: []int64{2}})
	for _, path := range []string{"/accounts/nope/upstream-policy", "/accounts/8/upstream-policy?group_id=0", "/accounts/8/upstream-policy?group_id=nope"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		newPolicyDiagnosticsRouter(handler).ServeHTTP(recorder, request)
		require.Equal(t, http.StatusBadRequest, recorder.Code, path)
	}
}

func TestGetUpstreamPolicyDiagnosticsRejectsGroupNotBoundToAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newPolicyDiagnosticsHandlerWithService(&policyDiagnosticsAdminService{
		account: &service.Account{ID: 8, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, GroupIDs: []int64{2}},
		group:   &service.Group{ID: 9, Platform: service.PlatformAnthropic, Status: service.StatusActive},
	})
	recorder := httptest.NewRecorder()
	newPolicyDiagnosticsRouter(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/8/upstream-policy?group_id=9", nil))
	require.Equal(t, http.StatusBadRequest, recorder.Code)
}

func TestGetUpstreamPolicyDiagnosticsReturnsNotFoundForMissingAccountOrGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"/accounts/8/upstream-policy", "/accounts/8/upstream-policy?group_id=2"} {
		handler := newPolicyDiagnosticsHandlerWithService(&policyDiagnosticsAdminService{})
		recorder := httptest.NewRecorder()
		newPolicyDiagnosticsRouter(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, recorder.Code, path)
	}
	adminSvc := &policyDiagnosticsAdminService{
		account: &service.Account{ID: 8, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, GroupIDs: []int64{2}},
	}
	handler := newPolicyDiagnosticsHandlerWithService(adminSvc)
	recorder := httptest.NewRecorder()
	newPolicyDiagnosticsRouter(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/8/upstream-policy?group_id=2", nil))
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestGetUpstreamPolicyDiagnosticsUsesLoadedBoundGroup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := &policyDiagnosticsAdminService{
		account: &service.Account{ID: 8, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, GroupIDs: []int64{2}},
		group:   &service.Group{ID: 2, DefaultPassthroughProfile: "transparent"},
	}
	handler := newPolicyDiagnosticsHandlerWithService(adminSvc)
	handler.SetUpstreamPolicySettingService(service.NewSettingService(&diagnosticSettingRepo{}, &config.Config{}))
	recorder := httptest.NewRecorder()
	newPolicyDiagnosticsRouter(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/8/upstream-policy?group_id=2", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope struct {
		Data service.UpstreamPolicyExplanation `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	require.NotNil(t, envelope.Data.GroupID)
	require.EqualValues(t, 2, *envelope.Data.GroupID)
	require.NotNil(t, envelope.Data.Candidate)
	require.Equal(t, service.ProfileTransparent, envelope.Data.Candidate.Profile)
}

func TestGetUpstreamPolicyDiagnosticsDoesNotSerializeAccountSecretsOrNames(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("SUB2API_UPSTREAM_POLICY_V1_ENABLED", "true")
	account := &service.Account{
		ID: 8, Name: "ACCOUNT_NAME_SENTINEL", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "API_KEY_SENTINEL", "base_url": "BASE_URL_SENTINEL"},
		Extra:       map[string]any{"upstream_passthrough": map[string]any{"profile": "transparent"}},
	}
	handler := newPolicyDiagnosticsHandler(t, account)
	handler.SetUpstreamPolicySettingService(service.NewSettingService(&diagnosticSettingRepo{}, &config.Config{}))
	recorder := httptest.NewRecorder()
	newPolicyDiagnosticsRouter(handler).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/accounts/8/upstream-policy", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	body, err := json.Marshal(envelope["data"])
	require.NoError(t, err)
	serialized := string(body)
	for _, sentinel := range []string{"ACCOUNT_NAME_SENTINEL", "API_KEY_SENTINEL", "BASE_URL_SENTINEL"} {
		require.NotContains(t, serialized, sentinel)
	}
	require.NotContains(t, serialized, "credentials")
	require.NotContains(t, serialized, "name")
}

func newPolicyDiagnosticsHandler(t *testing.T, account *service.Account) *AccountHandler {
	t.Helper()
	return newPolicyDiagnosticsHandlerWithService(&policyDiagnosticsAdminService{account: account})
}

func newPolicyDiagnosticsHandlerWithService(adminSvc service.AdminService) *AccountHandler {
	return NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

func newPolicyDiagnosticsRouter(handler *AccountHandler) *gin.Engine {
	router := gin.New()
	router.GET("/accounts/:id/upstream-policy", handler.GetUpstreamPolicyDiagnostics)
	return router
}

type diagnosticSettingRepo struct{}

type policyDiagnosticsAdminService struct {
	service.AdminService
	account *service.Account
	group   *service.Group
}

func (s *policyDiagnosticsAdminService) GetAccount(_ context.Context, _ int64) (*service.Account, error) {
	return s.account, nil
}

func (s *policyDiagnosticsAdminService) GetGroup(_ context.Context, _ int64) (*service.Group, error) {
	return s.group, nil
}

func (*diagnosticSettingRepo) Get(_ context.Context, _ string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}
func (*diagnosticSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyUpstreamPolicyV1Enabled {
		return "true", nil
	}
	return "", service.ErrSettingNotFound
}
func (*diagnosticSettingRepo) Set(context.Context, string, string) error { return nil }
func (*diagnosticSettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, nil
}
func (*diagnosticSettingRepo) SetMultiple(context.Context, map[string]string) error { return nil }
func (*diagnosticSettingRepo) GetAll(context.Context) (map[string]string, error)    { return nil, nil }
func (*diagnosticSettingRepo) Delete(context.Context, string) error                 { return nil }
