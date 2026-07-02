package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRegistration struct {
	Routes    []managementRoute    `json:"routes,omitempty"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Description string `json:"Description,omitempty"`
}

type managementResource struct {
	Path        string `json:"Path"`
	Menu        string `json:"Menu"`
	Description string `json:"Description"`
}

type managementRequest struct {
	Method string     `json:"Method,omitempty"`
	Path   string     `json:"Path,omitempty"`
	Query  url.Values `json:"Query,omitempty"`
	Body   []byte     `json:"Body,omitempty"`
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	if len(bytesTrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	if strings.HasSuffix(req.Path, "/release") {
		return handleRelease(req)
	}
	if req.Path != "" && !strings.HasSuffix(req.Path, "/status") && req.Path != "/status" {
		return okEnvelope(pluginapi.ManagementResponse{
			StatusCode: http.StatusNotFound,
			Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
			Body:       []byte(`{"error":"未找到"}`),
		})
	}
	body, err := json.MarshalIndent(globalLimiter.status(time.Now()), "", "  ")
	if err != nil {
		return nil, err
	}
	return okEnvelope(pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	})
}

func handleRelease(req managementRequest) ([]byte, error) {
	if !strings.EqualFold(req.Method, http.MethodPost) {
		return okEnvelope(jsonManagementResponse(http.StatusMethodNotAllowed, map[string]any{
			"error": "请求方法不允许",
		}))
	}
	releaseReq, err := releaseRequestFromManagement(req)
	if err != nil {
		return okEnvelope(jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error": err.Error(),
		}))
	}
	if !releaseReq.All && strings.TrimSpace(releaseReq.SlotID) == "" && strings.TrimSpace(releaseReq.AuthID) == "" && strings.TrimSpace(releaseReq.AuthIndex) == "" && strings.TrimSpace(releaseReq.FileKey) == "" {
		return okEnvelope(jsonManagementResponse(http.StatusBadRequest, map[string]any{
			"error": "必须提供 all、slot_id、auth_id、auth_index 或 file_key 之一",
		}))
	}
	resp := globalLimiter.releaseManual(releaseReq, time.Now())
	return okEnvelope(jsonManagementResponse(http.StatusOK, resp))
}

func releaseRequestFromManagement(req managementRequest) (releaseRequest, error) {
	var out releaseRequest
	if len(bytesTrimSpace(req.Body)) > 0 {
		if err := json.Unmarshal(req.Body, &out); err != nil {
			return releaseRequest{}, fmt.Errorf("decode release request body: %w", err)
		}
	}
	if req.Query == nil {
		return out, nil
	}
	if raw := strings.TrimSpace(req.Query.Get("all")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return releaseRequest{}, fmt.Errorf("all 必须是布尔值")
		}
		out.All = parsed
	}
	if raw := strings.TrimSpace(req.Query.Get("slot_id")); raw != "" {
		out.SlotID = raw
	}
	if raw := strings.TrimSpace(req.Query.Get("auth_id")); raw != "" {
		out.AuthID = raw
	}
	if raw := strings.TrimSpace(req.Query.Get("auth_index")); raw != "" {
		out.AuthIndex = raw
	}
	if raw := strings.TrimSpace(req.Query.Get("file_key")); raw != "" {
		out.FileKey = raw
	}
	return out, nil
}

func jsonManagementResponse(statusCode int, payload any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"error":"failed to encode response"}`)
		statusCode = http.StatusInternalServerError
	}
	return pluginapi.ManagementResponse{
		StatusCode: statusCode,
		Headers:    http.Header{"content-type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}
