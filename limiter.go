package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginName             = "auth-concurrency-limiter"
	strategyRoundRobin     = "round-robin"
	strategyFillFirst      = "fill-first"
	defaultSlotTTL         = 15 * time.Minute
	defaultRefreshInterval = 30 * time.Second
	defaultDenyStatus      = 429
)

var errHostCallbackUnavailable = errors.New("host callback is unavailable")

var globalLimiter = newLimiter()

var callHostRPC = func(method string, payload any) (json.RawMessage, error) {
	return nil, fmt.Errorf("%w: %s", errHostCallbackUnavailable, method)
}

type rawPluginConfig struct {
	DefaultLimit        int            `yaml:"default_limit"`
	SlotTTL             any            `yaml:"slot_ttl"`
	Strategy            string         `yaml:"strategy"`
	DenyStatus          int            `yaml:"deny_status"`
	Limits              map[string]int `yaml:"limits"`
	AuthRefreshInterval any            `yaml:"auth_refresh_interval"`
	ReadAuthLimits      *bool          `yaml:"read_auth_limits"`
}

type runtimeConfig struct {
	DefaultLimit        int
	SlotTTL             time.Duration
	Strategy            string
	DenyStatus          int
	AuthRefreshInterval time.Duration
	ReadAuthLimits      bool
	Limits              map[string]int
	RawLimits           map[string]int
}

type authRecord struct {
	ID       string `json:"id,omitempty"`
	Index    string `json:"auth_index,omitempty"`
	Name     string `json:"name,omitempty"`
	Path     string `json:"path,omitempty"`
	Provider string `json:"provider,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	HasLimit bool   `json:"has_limit,omitempty"`
}

type authListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type slot struct {
	ID         string    `json:"id"`
	AuthID     string    `json:"auth_id,omitempty"`
	AuthIndex  string    `json:"auth_index,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	Model      string    `json:"model,omitempty"`
	FileKey    string    `json:"file_key"`
	DisplayKey string    `json:"display_key"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type decoratedCandidate struct {
	Candidate     pluginapi.SchedulerAuthCandidate
	FileKey       string
	DisplayKey    string
	Limit         int
	LimitSource   string
	AuthIndex     string
	CurrentSlots  int
	HasLimit      bool
	PositiveLimit bool
}

type limiter struct {
	mu                 sync.Mutex
	cfg                runtimeConfig
	slots              map[string]map[string]slot
	authLookup         map[string]authRecord
	auths              map[string]authRecord
	lastAuthRefresh    time.Time
	lastAuthRefreshErr string
	refreshing         bool
	sequence           uint64
	rrCursor           map[string]int
}

type schedulerReject struct {
	Code       string
	Message    string
	HTTPStatus int
	Retryable  bool
}

type statusResponse struct {
	Name                string         `json:"name"`
	Config              statusConfig   `json:"config"`
	Buckets             []bucketStatus `json:"buckets"`
	Auths               []authRecord   `json:"auths"`
	LastAuthRefresh     string         `json:"last_auth_refresh,omitempty"`
	LastAuthRefreshErr  string         `json:"last_auth_refresh_error,omitempty"`
	HostAuthCacheSize   int            `json:"host_auth_cache_size"`
	ActiveSlotCount     int            `json:"active_slot_count"`
	ConfiguredLimitKeys []string       `json:"configured_limit_keys,omitempty"`
	ImplementationNotes []string       `json:"implementation_notes,omitempty"`
}

type releaseRequest struct {
	All       bool   `json:"all,omitempty"`
	SlotID    string `json:"slot_id,omitempty"`
	AuthID    string `json:"auth_id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	FileKey   string `json:"file_key,omitempty"`
}

type releaseResponse struct {
	Released int    `json:"released"`
	Reason   string `json:"reason,omitempty"`
}

type statusConfig struct {
	DefaultLimit        int               `json:"default_limit"`
	SlotTTLSeconds      int64             `json:"slot_ttl_seconds"`
	Strategy            string            `json:"strategy"`
	DenyStatus          int               `json:"deny_status"`
	AuthRefreshInterval int64             `json:"auth_refresh_interval_seconds"`
	ReadAuthLimits      bool              `json:"read_auth_limits"`
	Limits              map[string]int    `json:"limits,omitempty"`
	RawLimits           map[string]int    `json:"raw_limits,omitempty"`
	NormalizedLimits    map[string]string `json:"normalized_limits,omitempty"`
}

type bucketStatus struct {
	FileKey    string       `json:"file_key"`
	DisplayKey string       `json:"display_key,omitempty"`
	Count      int          `json:"count"`
	Slots      []slotStatus `json:"slots,omitempty"`
}

type slotStatus struct {
	ID               string `json:"slot_id"`
	AuthID           string `json:"auth_id,omitempty"`
	AuthIndex        string `json:"auth_index,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	AcquiredAt       string `json:"acquired_at"`
	ExpiresAt        string `json:"expires_at"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

func newLimiter() *limiter {
	return &limiter{
		cfg:        defaultRuntimeConfig(),
		slots:      make(map[string]map[string]slot),
		authLookup: make(map[string]authRecord),
		auths:      make(map[string]authRecord),
		rrCursor:   make(map[string]int),
	}
}

func defaultRuntimeConfig() runtimeConfig {
	return runtimeConfig{
		SlotTTL:             defaultSlotTTL,
		Strategy:            strategyRoundRobin,
		DenyStatus:          defaultDenyStatus,
		AuthRefreshInterval: defaultRefreshInterval,
		ReadAuthLimits:      true,
		Limits:              make(map[string]int),
		RawLimits:           make(map[string]int),
	}
}

func decodePluginConfig(raw []byte) (runtimeConfig, error) {
	cfg := defaultRuntimeConfig()
	if len(bytesTrimSpace(raw)) == 0 {
		return cfg, nil
	}

	var decoded rawPluginConfig
	if err := yaml.Unmarshal(raw, &decoded); err != nil {
		return runtimeConfig{}, err
	}

	cfg.DefaultLimit = decoded.DefaultLimit
	if decoded.SlotTTL != nil {
		ttl, err := parseDurationValue(decoded.SlotTTL, defaultSlotTTL)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("slot_ttl: %w", err)
		}
		cfg.SlotTTL = ttl
	}
	if cfg.SlotTTL <= 0 {
		cfg.SlotTTL = defaultSlotTTL
	}

	strategy := strings.ToLower(strings.TrimSpace(decoded.Strategy))
	switch strategy {
	case "", strategyRoundRobin:
		cfg.Strategy = strategyRoundRobin
	case strategyFillFirst:
		cfg.Strategy = strategyFillFirst
	default:
		return runtimeConfig{}, fmt.Errorf("strategy must be %q or %q", strategyRoundRobin, strategyFillFirst)
	}

	if decoded.DenyStatus > 0 {
		cfg.DenyStatus = decoded.DenyStatus
	}
	if cfg.DenyStatus <= 0 {
		cfg.DenyStatus = defaultDenyStatus
	}

	if decoded.AuthRefreshInterval != nil {
		interval, err := parseDurationValue(decoded.AuthRefreshInterval, defaultRefreshInterval)
		if err != nil {
			return runtimeConfig{}, fmt.Errorf("auth_refresh_interval: %w", err)
		}
		cfg.AuthRefreshInterval = interval
	}
	if cfg.AuthRefreshInterval < 0 {
		cfg.AuthRefreshInterval = 0
	}
	if decoded.ReadAuthLimits != nil {
		cfg.ReadAuthLimits = *decoded.ReadAuthLimits
	}

	cfg.RawLimits = cloneIntMap(decoded.Limits)
	cfg.Limits = make(map[string]int, len(decoded.Limits)*2)
	for key, limit := range decoded.Limits {
		for _, lookupKey := range lookupVariants(key) {
			cfg.Limits[lookupKey] = limit
		}
	}
	return cfg, nil
}

func parseDurationValue(raw any, fallback time.Duration) (time.Duration, error) {
	switch value := raw.(type) {
	case nil:
		return fallback, nil
	case time.Duration:
		return value, nil
	case int:
		return time.Duration(value) * time.Second, nil
	case int64:
		return time.Duration(value) * time.Second, nil
	case float64:
		return time.Duration(value * float64(time.Second)), nil
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return fallback, nil
		}
		if duration, err := time.ParseDuration(text); err == nil {
			return duration, nil
		}
		seconds, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		return time.Duration(seconds * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("invalid duration value %T", raw)
	}
}

func (l *limiter) configure(cfg runtimeConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cfg = cfg
	l.cleanupExpiredLocked(time.Now())
}

func (l *limiter) shutdown() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.slots = make(map[string]map[string]slot)
	l.authLookup = make(map[string]authRecord)
	l.auths = make(map[string]authRecord)
	l.rrCursor = make(map[string]int)
	l.lastAuthRefresh = time.Time{}
	l.lastAuthRefreshErr = ""
	l.refreshing = false
}

func (l *limiter) maybeRefreshAuths(now time.Time) {
	l.mu.Lock()
	cfg := l.cfg
	if cfg.AuthRefreshInterval <= 0 {
		l.mu.Unlock()
		return
	}
	if l.refreshing || (!l.lastAuthRefresh.IsZero() && now.Sub(l.lastAuthRefresh) < cfg.AuthRefreshInterval) {
		l.mu.Unlock()
		return
	}
	l.refreshing = true
	l.mu.Unlock()

	records, err := fetchHostAuthRecords(cfg)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil || len(records) > 0 {
		l.auths = records
		l.authLookup = buildAuthLookup(records)
	}
	if err == nil {
		l.lastAuthRefreshErr = ""
	} else {
		l.lastAuthRefreshErr = err.Error()
	}
	l.lastAuthRefresh = now
	l.refreshing = false
}

func fetchHostAuthRecords(cfg runtimeConfig) (map[string]authRecord, error) {
	raw, err := callHostRPC(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var list authListResponse
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", err)
	}

	records := make(map[string]authRecord, len(list.Files))
	var firstReadErr error
	for _, entry := range list.Files {
		rec := authRecord{
			ID:       strings.TrimSpace(entry.ID),
			Index:    strings.TrimSpace(entry.AuthIndex),
			Name:     strings.TrimSpace(entry.Name),
			Path:     strings.TrimSpace(entry.Path),
			Provider: strings.TrimSpace(entry.Provider),
		}
		if rec.Provider == "" {
			rec.Provider = strings.TrimSpace(entry.Type)
		}
		if rec.ID == "" && rec.Index == "" && rec.Name == "" && rec.Path == "" {
			continue
		}
		if cfg.ReadAuthLimits && !entry.RuntimeOnly && rec.Index != "" && rec.Path != "" {
			limit, ok, errLimit := fetchAuthJSONLimit(rec.Index)
			if errLimit != nil && firstReadErr == nil {
				firstReadErr = errLimit
			}
			if ok {
				rec.Limit = limit
				rec.HasLimit = true
			}
		}
		key := rec.ID
		if key == "" {
			key = rec.Index
		}
		if key == "" {
			key = rec.Path
		}
		if key == "" {
			key = rec.Name
		}
		records[key] = rec
	}
	if firstReadErr != nil {
		return records, firstReadErr
	}
	return records, nil
}

func fetchAuthJSONLimit(authIndex string) (int, bool, error) {
	raw, err := callHostRPC(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return 0, false, err
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, false, fmt.Errorf("decode host.auth.get %s: %w", authIndex, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(resp.JSON, &obj); err != nil {
		return 0, false, fmt.Errorf("decode auth json %s: %w", authIndex, err)
	}
	limit, ok := limitFromAnyMap(obj)
	return limit, ok, nil
}

func buildAuthLookup(records map[string]authRecord) map[string]authRecord {
	out := make(map[string]authRecord, len(records)*6)
	for _, rec := range records {
		for _, key := range authRecordKeys(rec) {
			out[key] = rec
		}
	}
	return out
}

func authRecordKeys(rec authRecord) []string {
	keys := make([]string, 0, 8)
	add := func(value string) {
		for _, key := range lookupVariants(value) {
			keys = append(keys, key)
		}
	}
	add(rec.ID)
	add(rec.Index)
	add(rec.Name)
	add(rec.Path)
	return uniqueStrings(keys)
}

func (l *limiter) pick(req pluginapi.SchedulerPickRequest, now time.Time) (pluginapi.SchedulerPickResponse, *schedulerReject) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupExpiredLocked(now)
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	decorated := make([]decoratedCandidate, 0, len(req.Candidates))
	hasPositiveLimit := false
	for _, candidate := range req.Candidates {
		item := l.decorateCandidateLocked(candidate)
		decorated = append(decorated, item)
		if item.PositiveLimit {
			hasPositiveLimit = true
		}
	}
	if !hasPositiveLimit {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}

	available := make([]bool, len(decorated))
	availableCount := 0
	for i := range decorated {
		item := &decorated[i]
		if item.Limit <= 0 || item.CurrentSlots < item.Limit {
			available[i] = true
			availableCount++
		}
	}
	if availableCount == 0 {
		return pluginapi.SchedulerPickResponse{}, &schedulerReject{
			Code:       "auth_concurrency_exhausted",
			Message:    "所有候选认证文件都已达到并发上限",
			HTTPStatus: l.cfg.DenyStatus,
			Retryable:  true,
		}
	}

	chosenIndex := l.chooseCandidateLocked(req, decorated, available)
	if chosenIndex < 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	chosen := decorated[chosenIndex]
	if chosen.Limit > 0 {
		l.acquireSlotLocked(chosen, req, now)
	}
	return pluginapi.SchedulerPickResponse{
		AuthID:  strings.TrimSpace(chosen.Candidate.ID),
		Handled: true,
	}, nil
}

func (l *limiter) decorateCandidateLocked(candidate pluginapi.SchedulerAuthCandidate) decoratedCandidate {
	rec, _ := l.lookupAuthRecordLocked(candidate)
	fileKey, displayKey := candidateFileKey(candidate, rec)
	keys := candidateLimitKeys(candidate, rec, fileKey, displayKey)

	limit, source, hasLimit := l.lookupConfiguredLimitLocked(keys)
	if !hasLimit {
		if rec.HasLimit {
			limit = rec.Limit
			source = "auth_json"
			hasLimit = true
		} else if metadataLimit, ok := limitFromAnyMap(candidate.Metadata); ok {
			limit = metadataLimit
			source = "candidate_metadata"
			hasLimit = true
		} else {
			limit = l.cfg.DefaultLimit
			source = "default"
			hasLimit = l.cfg.DefaultLimit > 0
		}
	}

	current := 0
	if fileKey != "" {
		current = len(l.slots[fileKey])
	}
	return decoratedCandidate{
		Candidate:     candidate,
		FileKey:       fileKey,
		DisplayKey:    displayKey,
		Limit:         limit,
		LimitSource:   source,
		AuthIndex:     rec.Index,
		CurrentSlots:  current,
		HasLimit:      hasLimit,
		PositiveLimit: limit > 0,
	}
}

func (l *limiter) lookupAuthRecordLocked(candidate pluginapi.SchedulerAuthCandidate) (authRecord, bool) {
	for _, key := range candidateLookupKeys(candidate) {
		if rec, ok := l.authLookup[key]; ok {
			return rec, true
		}
	}
	return authRecord{}, false
}

func candidateLookupKeys(candidate pluginapi.SchedulerAuthCandidate) []string {
	keys := make([]string, 0, 6)
	add := func(value string) {
		keys = append(keys, lookupVariants(value)...)
	}
	add(candidate.ID)
	if attrs := candidate.Attributes; len(attrs) > 0 {
		add(attrs["path"])
		add(attrs["source"])
		add(attrs["virtual_source"])
		add(attrs["auth_index"])
	}
	return uniqueStrings(keys)
}

func candidateFileKey(candidate pluginapi.SchedulerAuthCandidate, rec authRecord) (string, string) {
	values := []string{
		rec.Path,
		candidate.Attributes["path"],
		candidate.Attributes["virtual_source"],
		candidate.Attributes["source"],
		rec.Name,
		rec.ID,
		candidate.ID,
	}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		return normalizeLookupKey(value), value
	}
	return "", ""
}

func candidateLimitKeys(candidate pluginapi.SchedulerAuthCandidate, rec authRecord, fileKey, displayKey string) []string {
	keys := make([]string, 0, 16)
	add := func(value string) {
		keys = append(keys, lookupVariants(value)...)
	}
	add(candidate.ID)
	add(rec.ID)
	add(rec.Index)
	add(rec.Name)
	add(rec.Path)
	add(displayKey)
	add(fileKey)
	if attrs := candidate.Attributes; len(attrs) > 0 {
		add(attrs["path"])
		add(attrs["source"])
		add(attrs["virtual_source"])
		add(attrs["auth_index"])
	}
	return uniqueStrings(keys)
}

func (l *limiter) lookupConfiguredLimitLocked(keys []string) (int, string, bool) {
	for _, key := range keys {
		if limit, ok := l.cfg.Limits[key]; ok {
			return limit, "config", true
		}
	}
	return 0, "", false
}

func (l *limiter) chooseCandidateLocked(req pluginapi.SchedulerPickRequest, decorated []decoratedCandidate, available []bool) int {
	if len(decorated) == 0 {
		return -1
	}
	if l.cfg.Strategy == strategyFillFirst {
		for i := range decorated {
			if available[i] {
				return i
			}
		}
		return -1
	}

	cursorKey := schedulerCursorKey(req)
	start := l.rrCursor[cursorKey]
	if start < 0 {
		start = 0
	}
	if len(decorated) > 0 {
		start %= len(decorated)
	}
	for offset := 0; offset < len(decorated); offset++ {
		index := (start + offset) % len(decorated)
		if available[index] {
			l.rrCursor[cursorKey] = (index + 1) % len(decorated)
			return index
		}
	}
	return -1
}

func schedulerCursorKey(req pluginapi.SchedulerPickRequest) string {
	providers := append([]string(nil), req.Providers...)
	if strings.TrimSpace(req.Provider) != "" {
		providers = append(providers, req.Provider)
	}
	for i := range providers {
		providers[i] = strings.ToLower(strings.TrimSpace(providers[i]))
	}
	sort.Strings(providers)
	return strings.Join(uniqueStrings(providers), ",") + "|" + strings.TrimSpace(req.Model)
}

func (l *limiter) acquireSlotLocked(chosen decoratedCandidate, req pluginapi.SchedulerPickRequest, now time.Time) {
	if chosen.FileKey == "" {
		return
	}
	l.sequence++
	slotID := strconv.FormatInt(now.UnixNano(), 36) + "-" + strconv.FormatUint(l.sequence, 36)
	if l.slots[chosen.FileKey] == nil {
		l.slots[chosen.FileKey] = make(map[string]slot)
	}
	l.slots[chosen.FileKey][slotID] = slot{
		ID:         slotID,
		AuthID:     strings.TrimSpace(chosen.Candidate.ID),
		AuthIndex:  strings.TrimSpace(chosen.AuthIndex),
		Provider:   strings.TrimSpace(chosen.Candidate.Provider),
		Model:      strings.TrimSpace(req.Model),
		FileKey:    chosen.FileKey,
		DisplayKey: chosen.DisplayKey,
		AcquiredAt: now,
		ExpiresAt:  now.Add(l.cfg.SlotTTL),
	}
}

func (l *limiter) release(record pluginapi.UsageRecord, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupExpiredLocked(now)
	authID := strings.TrimSpace(record.AuthID)
	authIndex := strings.TrimSpace(record.AuthIndex)
	if authID == "" && authIndex == "" {
		return false
	}

	bestKey := ""
	bestSlotID := ""
	var bestSlot slot
	found := false
	for fileKey, slots := range l.slots {
		for slotID, candidate := range slots {
			if authID != "" && candidate.AuthID != authID {
				continue
			}
			if authID == "" && authIndex != "" && candidate.AuthIndex != authIndex {
				continue
			}
			if !found || candidate.AcquiredAt.Before(bestSlot.AcquiredAt) {
				bestKey = fileKey
				bestSlotID = slotID
				bestSlot = candidate
				found = true
			}
		}
	}
	if !found {
		return false
	}
	delete(l.slots[bestKey], bestSlotID)
	if len(l.slots[bestKey]) == 0 {
		delete(l.slots, bestKey)
	}
	return true
}

func (l *limiter) releaseManual(req releaseRequest, now time.Time) releaseResponse {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.cleanupExpiredLocked(now)
	req.SlotID = strings.TrimSpace(req.SlotID)
	req.AuthID = strings.TrimSpace(req.AuthID)
	req.AuthIndex = strings.TrimSpace(req.AuthIndex)
	req.FileKey = strings.TrimSpace(req.FileKey)

	if req.All {
		count := 0
		for _, slots := range l.slots {
			count += len(slots)
		}
		l.slots = make(map[string]map[string]slot)
		return releaseResponse{Released: count, Reason: "all"}
	}
	if req.SlotID != "" {
		if l.releaseSlotIDLocked(req.SlotID) {
			return releaseResponse{Released: 1, Reason: "slot_id"}
		}
		return releaseResponse{Released: 0, Reason: "slot_id_not_found"}
	}
	if req.FileKey != "" {
		return releaseResponse{Released: l.releaseFileKeyLocked(req.FileKey), Reason: "file_key"}
	}
	if req.AuthID != "" || req.AuthIndex != "" {
		return releaseResponse{Released: l.releaseMatchingAuthLocked(req.AuthID, req.AuthIndex), Reason: "auth"}
	}
	return releaseResponse{Released: 0, Reason: "missing_selector"}
}

func (l *limiter) releaseSlotIDLocked(slotID string) bool {
	for fileKey, slots := range l.slots {
		if _, ok := slots[slotID]; !ok {
			continue
		}
		delete(slots, slotID)
		if len(slots) == 0 {
			delete(l.slots, fileKey)
		}
		return true
	}
	return false
}

func (l *limiter) releaseFileKeyLocked(fileKey string) int {
	keys := lookupVariants(fileKey)
	if len(keys) == 0 {
		return 0
	}
	selectors := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		selectors[key] = struct{}{}
	}
	released := 0
	for key, slots := range l.slots {
		matched := false
		if _, ok := selectors[key]; ok {
			matched = true
		}
		if !matched {
			for _, item := range slots {
				for _, displayKey := range lookupVariants(item.DisplayKey) {
					if _, ok := selectors[displayKey]; ok {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
		}
		if matched {
			released += len(slots)
			delete(l.slots, key)
		}
	}
	return released
}

func (l *limiter) releaseMatchingAuthLocked(authID, authIndex string) int {
	released := 0
	for fileKey, slots := range l.slots {
		for slotID, candidate := range slots {
			if authID != "" && candidate.AuthID != authID {
				continue
			}
			if authIndex != "" && candidate.AuthIndex != authIndex {
				continue
			}
			delete(slots, slotID)
			released++
		}
		if len(slots) == 0 {
			delete(l.slots, fileKey)
		}
	}
	return released
}

func (l *limiter) cleanupExpiredLocked(now time.Time) {
	for fileKey, slots := range l.slots {
		for slotID, item := range slots {
			if !item.ExpiresAt.IsZero() && !now.Before(item.ExpiresAt) {
				delete(slots, slotID)
			}
		}
		if len(slots) == 0 {
			delete(l.slots, fileKey)
		}
	}
}

func (l *limiter) status(now time.Time) statusResponse {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanupExpiredLocked(now)

	buckets := make([]bucketStatus, 0, len(l.slots))
	activeSlots := 0
	for fileKey, bucket := range l.slots {
		status := bucketStatus{FileKey: fileKey, Count: len(bucket)}
		for _, item := range bucket {
			if status.DisplayKey == "" {
				status.DisplayKey = item.DisplayKey
			}
			activeSlots++
			expiresIn := int64(0)
			if !item.ExpiresAt.IsZero() && now.Before(item.ExpiresAt) {
				expiresIn = int64(item.ExpiresAt.Sub(now).Seconds())
			}
			status.Slots = append(status.Slots, slotStatus{
				ID:               item.ID,
				AuthID:           item.AuthID,
				AuthIndex:        item.AuthIndex,
				Provider:         item.Provider,
				Model:            item.Model,
				AcquiredAt:       item.AcquiredAt.Format(time.RFC3339),
				ExpiresAt:        item.ExpiresAt.Format(time.RFC3339),
				ExpiresInSeconds: expiresIn,
			})
		}
		sort.Slice(status.Slots, func(i, j int) bool {
			return status.Slots[i].AcquiredAt < status.Slots[j].AcquiredAt
		})
		buckets = append(buckets, status)
	}
	sort.Slice(buckets, func(i, j int) bool {
		return buckets[i].FileKey < buckets[j].FileKey
	})

	auths := make([]authRecord, 0, len(l.auths))
	for _, rec := range l.auths {
		auths = append(auths, rec)
	}
	sort.Slice(auths, func(i, j int) bool {
		if auths[i].Name != auths[j].Name {
			return auths[i].Name < auths[j].Name
		}
		return auths[i].ID < auths[j].ID
	})

	normalizedLimits := make(map[string]string, len(l.cfg.RawLimits))
	configuredKeys := make([]string, 0, len(l.cfg.RawLimits))
	for raw := range l.cfg.RawLimits {
		configuredKeys = append(configuredKeys, raw)
		normalizedLimits[raw] = strings.Join(lookupVariants(raw), ",")
	}
	sort.Strings(configuredKeys)

	lastRefresh := ""
	if !l.lastAuthRefresh.IsZero() {
		lastRefresh = l.lastAuthRefresh.Format(time.RFC3339)
	}
	return statusResponse{
		Name: pluginName,
		Config: statusConfig{
			DefaultLimit:        l.cfg.DefaultLimit,
			SlotTTLSeconds:      int64(l.cfg.SlotTTL.Seconds()),
			Strategy:            l.cfg.Strategy,
			DenyStatus:          l.cfg.DenyStatus,
			AuthRefreshInterval: int64(l.cfg.AuthRefreshInterval.Seconds()),
			ReadAuthLimits:      l.cfg.ReadAuthLimits,
			Limits:              cloneIntMap(l.cfg.RawLimits),
			RawLimits:           cloneIntMap(l.cfg.RawLimits),
			NormalizedLimits:    normalizedLimits,
		},
		Buckets:             buckets,
		Auths:               auths,
		LastAuthRefresh:     lastRefresh,
		LastAuthRefreshErr:  l.lastAuthRefreshErr,
		HostAuthCacheSize:   len(l.auths),
		ActiveSlotCount:     activeSlots,
		ConfiguredLimitKeys: configuredKeys,
		ImplementationNotes: []string{
			"纯插件模式会在 usage.handle 收到用量记录时释放槽位，并使用 slot_ttl 作为兜底过期时间。",
			"如果单次请求运行时间超过 slot_ttl，槽位可能会在请求真正完成前过期。",
		},
	}
}

func limitFromAnyMap(values map[string]any) (int, bool) {
	if len(values) == 0 {
		return 0, false
	}
	for _, key := range []string{"cpa_max_concurrency", "max_concurrency", "cpaMaxConcurrency", "maxConcurrency"} {
		if limit, ok := intFromAny(values[key]); ok {
			return limit, true
		}
	}
	return 0, false
}

func intFromAny(raw any) (int, bool) {
	switch value := raw.(type) {
	case nil:
		return 0, false
	case int:
		return value, true
	case int8:
		return int(value), true
	case int16:
		return int(value), true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	case uint:
		return int(value), true
	case uint8:
		return int(value), true
	case uint16:
		return int(value), true
	case uint32:
		return int(value), true
	case uint64:
		return int(value), true
	case float32:
		return int(value), true
	case float64:
		return int(value), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func lookupVariants(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	keys := []string{normalizeLookupKey(value)}
	base := filepath.Base(value)
	if base != "." && base != "" && base != value {
		keys = append(keys, normalizeLookupKey(base))
	}
	if strings.Contains(value, "\\") {
		slashBase := path.Base(strings.ReplaceAll(value, "\\", "/"))
		if slashBase != "." && slashBase != "" {
			keys = append(keys, normalizeLookupKey(slashBase))
		}
	}
	return uniqueStrings(keys)
}

func normalizeLookupKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.ReplaceAll(value, "\\", "/")
	if strings.Contains(value, "/") {
		value = path.Clean(value)
	}
	return strings.ToLower(value)
}

func uniqueStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}
