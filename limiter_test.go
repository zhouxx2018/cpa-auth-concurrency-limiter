package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestDecodePluginConfigNormalizesLimits(t *testing.T) {
	cfg, err := decodePluginConfig([]byte(`
default_limit: 1
slot_ttl: 2s
strategy: fill-first
deny_status: 503
auth_refresh_interval: 0
read_auth_limits: false
limits:
  "C:\\Auth\\Account-A.JSON": 3
`))
	if err != nil {
		t.Fatalf("decodePluginConfig() error = %v", err)
	}
	if cfg.DefaultLimit != 1 {
		t.Fatalf("DefaultLimit = %d, want 1", cfg.DefaultLimit)
	}
	if cfg.SlotTTL != 2*time.Second {
		t.Fatalf("SlotTTL = %s, want 2s", cfg.SlotTTL)
	}
	if cfg.Strategy != strategyFillFirst {
		t.Fatalf("Strategy = %q, want fill-first", cfg.Strategy)
	}
	if cfg.DenyStatus != 503 {
		t.Fatalf("DenyStatus = %d, want 503", cfg.DenyStatus)
	}
	if cfg.AuthRefreshInterval != 0 {
		t.Fatalf("AuthRefreshInterval = %s, want 0", cfg.AuthRefreshInterval)
	}
	if cfg.ReadAuthLimits {
		t.Fatal("ReadAuthLimits = true, want false")
	}
	if got := cfg.Limits["c:/auth/account-a.json"]; got != 3 {
		t.Fatalf("path limit = %d, want 3", got)
	}
	if got := cfg.Limits["account-a.json"]; got != 3 {
		t.Fatalf("base limit = %d, want 3", got)
	}
}

func TestPickAcquireRejectRelease(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 1
slot_ttl: 15m
strategy: fill-first
auth_refresh_interval: 0
`)
	req := schedulerRequest(candidate("auth-a", "C:\\auth\\a.json"))
	now := time.Unix(100, 0)

	resp, reject := l.pick(req, now)
	if reject != nil {
		t.Fatalf("first pick reject = %#v", reject)
	}
	if !resp.Handled || resp.AuthID != "auth-a" {
		t.Fatalf("first pick = %+v, want auth-a handled", resp)
	}

	_, reject = l.pick(req, now.Add(time.Second))
	if reject == nil {
		t.Fatal("second pick reject = nil, want exhausted")
	}
	if reject.HTTPStatus != defaultDenyStatus {
		t.Fatalf("reject status = %d, want %d", reject.HTTPStatus, defaultDenyStatus)
	}

	if !l.release(pluginapi.UsageRecord{AuthID: "auth-a"}, now.Add(2*time.Second)) {
		t.Fatal("release returned false, want true")
	}
	resp, reject = l.pick(req, now.Add(3*time.Second))
	if reject != nil || resp.AuthID != "auth-a" {
		t.Fatalf("pick after release = %+v reject=%#v, want auth-a", resp, reject)
	}
}

func TestExplicitFilenameLimitWithUnlimitedDefault(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 0
slot_ttl: 15m
auth_refresh_interval: 0
limits:
  account-a.json: 1
`)
	req := schedulerRequest(candidate("auth-a", "/var/lib/cpa/auth/account-a.json"))
	now := time.Unix(200, 0)

	resp, reject := l.pick(req, now)
	if reject != nil || resp.AuthID != "auth-a" {
		t.Fatalf("first pick = %+v reject=%#v, want auth-a", resp, reject)
	}
	_, reject = l.pick(req, now.Add(time.Second))
	if reject == nil {
		t.Fatal("second pick reject = nil, want exhausted")
	}

	unlimitedResp, unlimitedReject := l.pick(schedulerRequest(candidate("auth-b", "/var/lib/cpa/auth/account-b.json")), now)
	if unlimitedReject != nil {
		t.Fatalf("unlimited pick reject = %#v", unlimitedReject)
	}
	if unlimitedResp.Handled {
		t.Fatalf("unlimited pick handled = true, want false so built-in scheduler can decide")
	}
}

func TestVirtualSourceSharesBucket(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 1
slot_ttl: 15m
auth_refresh_interval: 0
`)
	req := schedulerRequest(
		candidateWithAttrs("virtual-a", map[string]string{"virtual_source": "/auth/group.json"}),
		candidateWithAttrs("virtual-b", map[string]string{"virtual_source": "/auth/group.json"}),
	)
	now := time.Unix(300, 0)

	resp, reject := l.pick(req, now)
	if reject != nil || resp.AuthID != "virtual-a" {
		t.Fatalf("first pick = %+v reject=%#v, want virtual-a", resp, reject)
	}
	_, reject = l.pick(req, now.Add(time.Second))
	if reject == nil {
		t.Fatal("second pick reject = nil, want exhausted shared bucket")
	}
}

func TestSlotTTLReleasesStaleSlot(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 1
slot_ttl: 1s
auth_refresh_interval: 0
`)
	req := schedulerRequest(candidate("auth-a", "/auth/a.json"))
	now := time.Unix(400, 0)

	if _, reject := l.pick(req, now); reject != nil {
		t.Fatalf("first pick reject = %#v", reject)
	}
	resp, reject := l.pick(req, now.Add(2*time.Second))
	if reject != nil || resp.AuthID != "auth-a" {
		t.Fatalf("pick after ttl = %+v reject=%#v, want auth-a", resp, reject)
	}
}

func TestRoundRobinSkipsFullCandidates(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 1
slot_ttl: 15m
strategy: round-robin
auth_refresh_interval: 0
`)
	req := schedulerRequest(
		candidate("auth-a", "/auth/a.json"),
		candidate("auth-b", "/auth/b.json"),
	)
	now := time.Unix(500, 0)

	resp, reject := l.pick(req, now)
	if reject != nil || resp.AuthID != "auth-a" {
		t.Fatalf("first pick = %+v reject=%#v, want auth-a", resp, reject)
	}
	resp, reject = l.pick(req, now.Add(time.Second))
	if reject != nil || resp.AuthID != "auth-b" {
		t.Fatalf("second pick = %+v reject=%#v, want auth-b", resp, reject)
	}
	_, reject = l.pick(req, now.Add(2*time.Second))
	if reject == nil {
		t.Fatal("third pick reject = nil, want exhausted")
	}
}

func TestAuthJSONLimitFromHostCache(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 0
slot_ttl: 15m
auth_refresh_interval: 0
`)
	rec := authRecord{ID: "auth-a", Name: "a.json", Path: "/auth/a.json", Limit: 1, HasLimit: true}
	l.mu.Lock()
	l.auths = map[string]authRecord{"auth-a": rec}
	l.authLookup = buildAuthLookup(l.auths)
	l.mu.Unlock()

	req := schedulerRequest(candidate("auth-a", ""))
	resp, reject := l.pick(req, time.Unix(600, 0))
	if reject != nil || resp.AuthID != "auth-a" {
		t.Fatalf("first pick = %+v reject=%#v, want auth-a", resp, reject)
	}
	_, reject = l.pick(req, time.Unix(601, 0))
	if reject == nil {
		t.Fatal("second pick reject = nil, want exhausted from auth JSON limit")
	}
}

func TestManualReleaseByAuthID(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 1
slot_ttl: 15m
auth_refresh_interval: 0
`)
	req := schedulerRequest(candidate("auth-a", "/auth/a.json"))
	now := time.Unix(700, 0)
	if _, reject := l.pick(req, now); reject != nil {
		t.Fatalf("pick reject = %#v", reject)
	}

	released := l.releaseManual(releaseRequest{AuthID: "auth-a"}, now.Add(time.Second))
	if released.Released != 1 {
		t.Fatalf("release by auth_id = %+v, want 1", released)
	}
	resp, reject := l.pick(req, now.Add(2*time.Second))
	if reject != nil || resp.AuthID != "auth-a" {
		t.Fatalf("pick after manual release = %+v reject=%#v, want auth-a", resp, reject)
	}
}

func TestManualReleaseByFilenameMatchesFullPathBucket(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 2
slot_ttl: 15m
auth_refresh_interval: 0
`)
	req := schedulerRequest(candidate("auth-a", "C:\\auth\\account-a.json"))
	now := time.Unix(800, 0)
	if _, reject := l.pick(req, now); reject != nil {
		t.Fatalf("first pick reject = %#v", reject)
	}
	if _, reject := l.pick(req, now.Add(time.Second)); reject != nil {
		t.Fatalf("second pick reject = %#v", reject)
	}

	released := l.releaseManual(releaseRequest{FileKey: "account-a.json"}, now.Add(2*time.Second))
	if released.Released != 2 {
		t.Fatalf("release by filename = %+v, want 2", released)
	}
	if status := l.status(now.Add(3 * time.Second)); status.ActiveSlotCount != 0 {
		t.Fatalf("ActiveSlotCount = %d, want 0", status.ActiveSlotCount)
	}
}

func TestManualReleaseBySlotID(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 1
slot_ttl: 15m
auth_refresh_interval: 0
`)
	now := time.Unix(900, 0)
	if _, reject := l.pick(schedulerRequest(candidate("auth-a", "/auth/a.json")), now); reject != nil {
		t.Fatalf("pick reject = %#v", reject)
	}

	slotID := onlySlotID(t, l)
	released := l.releaseManual(releaseRequest{SlotID: slotID}, now.Add(time.Second))
	if released.Released != 1 {
		t.Fatalf("release by slot_id = %+v, want 1", released)
	}
	if status := l.status(now.Add(2 * time.Second)); status.ActiveSlotCount != 0 {
		t.Fatalf("ActiveSlotCount = %d, want 0", status.ActiveSlotCount)
	}
}

func TestManualReleaseAll(t *testing.T) {
	l := configuredLimiter(t, `
default_limit: 1
slot_ttl: 15m
auth_refresh_interval: 0
`)
	now := time.Unix(1000, 0)
	if _, reject := l.pick(schedulerRequest(candidate("auth-a", "/auth/a.json")), now); reject != nil {
		t.Fatalf("pick auth-a reject = %#v", reject)
	}
	if _, reject := l.pick(schedulerRequest(candidate("auth-b", "/auth/b.json")), now); reject != nil {
		t.Fatalf("pick auth-b reject = %#v", reject)
	}

	released := l.releaseManual(releaseRequest{All: true}, now.Add(time.Second))
	if released.Released != 2 {
		t.Fatalf("release all = %+v, want 2", released)
	}
	if status := l.status(now.Add(2 * time.Second)); status.ActiveSlotCount != 0 {
		t.Fatalf("ActiveSlotCount = %d, want 0", status.ActiveSlotCount)
	}
}

func TestReleaseRequestFromManagementMergesBodyAndQuery(t *testing.T) {
	req, err := releaseRequestFromManagement(managementRequest{
		Body:  []byte(`{"auth_id":"auth-a"}`),
		Query: url.Values{"file_key": []string{"account-a.json"}, "all": []string{"false"}},
	})
	if err != nil {
		t.Fatalf("releaseRequestFromManagement() error = %v", err)
	}
	if req.AuthID != "auth-a" || req.FileKey != "account-a.json" || req.All {
		t.Fatalf("release request = %+v, want auth_id and file_key with all=false", req)
	}
}

func TestHandleReleaseReleasesGlobalLimiterSlot(t *testing.T) {
	previousLimiter := globalLimiter
	defer func() {
		globalLimiter = previousLimiter
	}()

	globalLimiter = configuredLimiter(t, `
default_limit: 1
slot_ttl: 15m
auth_refresh_interval: 0
`)
	now := time.Now()
	if _, reject := globalLimiter.pick(schedulerRequest(candidate("auth-a", "/auth/a.json")), now); reject != nil {
		t.Fatalf("pick reject = %#v", reject)
	}

	raw, err := handleRelease(managementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/auth-concurrency-limiter/release",
		Body:   []byte(`{"auth_id":"auth-a"}`),
	})
	if err != nil {
		t.Fatalf("handleRelease() error = %v", err)
	}
	resp := decodeManagementEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200 body=%s", resp.StatusCode, string(resp.Body))
	}
	var body releaseResponse
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("decode release response: %v", err)
	}
	if body.Released != 1 {
		t.Fatalf("Released = %d, want 1", body.Released)
	}
}

func configuredLimiter(t *testing.T, yamlText string) *limiter {
	t.Helper()
	cfg, err := decodePluginConfig([]byte(yamlText))
	if err != nil {
		t.Fatalf("decodePluginConfig() error = %v", err)
	}
	l := newLimiter()
	l.configure(cfg)
	return l
}

func schedulerRequest(candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	return pluginapi.SchedulerPickRequest{
		Provider:   "gemini",
		Providers:  []string{"gemini"},
		Model:      "test-model",
		Candidates: candidates,
	}
}

func candidate(id, filePath string) pluginapi.SchedulerAuthCandidate {
	attrs := map[string]string{}
	if filePath != "" {
		attrs["path"] = filePath
		attrs["source"] = filePath
	}
	return candidateWithAttrs(id, attrs)
}

func candidateWithAttrs(id string, attrs map[string]string) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{
		ID:         id,
		Provider:   "gemini",
		Status:     "active",
		Attributes: attrs,
	}
}

func onlySlotID(t *testing.T, l *limiter) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, slots := range l.slots {
		for slotID := range slots {
			return slotID
		}
	}
	t.Fatal("no slot found")
	return ""
}

func decodeManagementEnvelope(t *testing.T, raw []byte) pluginapi.ManagementResponse {
	t.Helper()
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not ok: %+v", env.Error)
	}
	var resp pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode management response: %v", err)
	}
	return resp
}
