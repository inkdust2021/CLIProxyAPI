package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	claudeQuotaMaxAccounts        = 128
	claudeQuotaMaxModels          = 8
	claudeQuotaProfileSamples     = 32
	claudeQuotaCalibrationSamples = 8
	claudeQuotaEvaluationLimit    = 64
)

type claudeQuotaReleaseContextKey struct{}
type claudeQuotaCountTokensContextKey struct{}

type claudeQuotaProfile struct {
	Work       []float64 `json:"work"`
	ObservedAt time.Time `json:"observed_at"`
}

type claudeQuotaWindow struct {
	Utilization float64   `json:"utilization"`
	Reset       time.Time `json:"reset"`
	ObservedAt  time.Time `json:"observed_at"`
	Ratios      []float64 `json:"ratios,omitempty"`
	PendingWork float64   `json:"pending_work"`
	paired      bool
}

type claudeQuotaAccount struct {
	Profiles    map[string]*claudeQuotaProfile `json:"profiles"`
	Windows     map[string]*claudeQuotaWindow  `json:"windows"`
	CompletedAt time.Time                      `json:"completed_at"`
	activeWork  float64
	active      int
}

// ClaudeQuotaPredictionEvaluation compares a prior one-request forecast with a later trusted quota header.
type ClaudeQuotaPredictionEvaluation struct {
	AccountKey           string    `json:"account_key"`
	Window               string    `json:"window"`
	Model                string    `json:"model"`
	Estimator            string    `json:"estimator,omitempty"`
	ObservedAt           time.Time `json:"observed_at"`
	Reset                time.Time `json:"reset"`
	BeforeUtilization    float64   `json:"before_utilization"`
	PredictedUtilization float64   `json:"predicted_utilization"`
	ActualUtilization    float64   `json:"actual_utilization"`
	PredictedWork        float64   `json:"predicted_work"`
	ActualWork           float64   `json:"actual_work"`
	CalibrationSamples   int       `json:"calibration_samples"`
}

type claudeQuotaPredictor struct {
	mu          sync.Mutex
	accounts    map[string]*claudeQuotaAccount
	evaluations []ClaudeQuotaPredictionEvaluation
	dir         string
	blocked     bool
}

func newClaudeQuotaPredictor() *claudeQuotaPredictor {
	return &claudeQuotaPredictor{accounts: make(map[string]*claudeQuotaAccount)}
}

func claudeQuotaAccountKey(auth *Auth) string {
	_, email := auth.AccountInfo()
	identity := strings.ToLower(strings.TrimSpace(email))
	if identity == "" {
		identity = "auth:" + auth.ID
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

// API price ratios are workload priors, not a subscription-quota formula.
// The scale is learned only from real quota changes for this account/window.
func claudeQuotaWork(model string, detail usage.Detail) (float64, bool) {
	model = canonicalClaudeQuotaModel(model)
	base, read := 0.0, 0.1
	for _, version := range []string{"4-5", "4-6", "4-7", "4-8", "5"} {
		if model == "claude-opus-"+version || strings.HasPrefix(model, "claude-opus-"+version+"-202") {
			base = 1
		}
	}
	if model == "claude-opus-5-5" || strings.HasPrefix(model, "claude-opus-5-5-") {
		base, read = .8, .05
	}
	if model == "claude-sonnet-4-5" || model == "claude-sonnet-4-6" || strings.HasPrefix(model, "claude-sonnet-4-5-202") || strings.HasPrefix(model, "claude-sonnet-4-6-202") {
		base = .6
	}
	if model == "claude-sonnet-5" {
		base = .4
	}
	if model == "claude-sonnet-5-5" || strings.HasPrefix(model, "claude-sonnet-5-5-") {
		base, read = .4, .05
	}
	if model == "claude-haiku-4-5" || strings.HasPrefix(model, "claude-haiku-4-5-202") {
		base = .2
	}
	if base == 0 || detail.InputTokens < 0 || detail.OutputTokens < 0 || detail.CacheReadTokens < 0 || detail.CacheCreationTokens < 0 || detail.CacheCreation5mTokens < 0 || detail.CacheCreation1hTokens < 0 {
		return 0, false
	}
	if detail.TokenBreakdown.Valid() && detail.TokenBreakdown.Quality != usage.TokenAccountingQualityComplete {
		return 0, false
	}
	writes5, writes1 := float64(detail.CacheCreation5mTokens), float64(detail.CacheCreation1hTokens)
	unknownWrites := float64(detail.CacheCreationTokens) - writes5 - writes1
	if unknownWrites < 0 {
		return 0, false
	}
	// Thinking is already included in Claude output_tokens. Unknown write TTL uses the upper prior.
	work := base * (float64(detail.InputTokens) + 5*float64(detail.OutputTokens) + read*float64(detail.CacheReadTokens) + 1.25*writes5 + 2*(writes1+unknownWrites))
	return work, work > 0 && !math.IsNaN(work) && !math.IsInf(work, 0)
}

func canonicalClaudeQuotaModel(model string) string {
	model = strings.ReplaceAll(strings.ToLower(canonicalModelKey(model)), ".", "-")
	if date := strings.Index(model, "-202"); date >= 0 {
		model = model[:date]
	}
	return model
}

func appendClaudeQuotaSample(samples []float64, value float64, limit int) []float64 {
	if len(samples) >= limit {
		samples = append(samples[:0], samples[len(samples)-limit+1:]...)
	}
	return append(samples, value)
}

func claudeQuotaUpperSample(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	values := append([]float64(nil), samples...)
	sort.Float64s(values)
	return values[int(math.Ceil(.9*float64(len(values))))-1]
}

// Cap an isolated rounded-header spike while retaining the upper estimate for stable ratios.
func claudeQuotaUpperRatio(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	values := append([]float64(nil), samples...)
	sort.Float64s(values)
	upper := values[int(math.Ceil(.9*float64(len(values))))-1]
	median := values[len(values)/2]
	if len(values)%2 == 0 {
		median = values[len(values)/2-1]/2 + median/2
	}
	return math.Min(upper, 2*median)
}

func (p *claudeQuotaPredictor) accountLocked(key string) *claudeQuotaAccount {
	if account := p.accounts[key]; account != nil {
		return account
	}
	if len(p.accounts) >= claudeQuotaMaxAccounts {
		oldestKey := ""
		var oldest time.Time
		for candidate, account := range p.accounts {
			if account.active == 0 && (oldestKey == "" || account.CompletedAt.Before(oldest)) {
				oldestKey, oldest = candidate, account.CompletedAt
			}
		}
		if oldestKey == "" {
			return nil
		}
		delete(p.accounts, oldestKey)
	}
	account := &claudeQuotaAccount{Profiles: make(map[string]*claudeQuotaProfile), Windows: make(map[string]*claudeQuotaWindow)}
	p.accounts[key] = account
	return account
}

func (p *claudeQuotaPredictor) seedLocked(account *claudeQuotaAccount, model string, work float64, at time.Time) {
	model = canonicalClaudeQuotaModel(model)
	profile := account.Profiles[model]
	if profile == nil {
		if len(account.Profiles) >= claudeQuotaMaxModels {
			oldestKey := ""
			var oldest time.Time
			for key, value := range account.Profiles {
				if oldestKey == "" || value.ObservedAt.Before(oldest) {
					oldestKey, oldest = key, value.ObservedAt
				}
			}
			delete(account.Profiles, oldestKey)
		}
		profile = &claudeQuotaProfile{}
		account.Profiles[model] = profile
	}
	profile.Work = appendClaudeQuotaSample(profile.Work, work, claudeQuotaProfileSamples)
	profile.ObservedAt = at
}

func (p *claudeQuotaPredictor) seed(auth *Auth, model string, detail usage.Detail, at time.Time) {
	work, ok := claudeQuotaWork(model, detail)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if account := p.accountLocked(claudeQuotaAccountKey(auth)); account != nil {
		p.seedLocked(account, model, work, at)
		if at.After(account.CompletedAt) {
			account.CompletedAt = at
		}
		p.persistLocked()
	}
}

func (p *claudeQuotaPredictor) observe(auth *Auth, record usage.Record) {
	model := record.ResponseModel
	if model == "" {
		model = record.Model
	}
	work, workOK := claudeQuotaWork(model, record.Detail)
	at := record.RequestedAt.Add(record.Latency)
	if record.RequestedAt.IsZero() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	accountKey := claudeQuotaAccountKey(auth)
	account := p.accountLocked(accountKey)
	if account == nil || at.Before(account.CompletedAt) {
		return
	}
	replay := record.Source == "claude-cache-keepalive"
	serial := !replay && !record.RequestedAt.Before(account.CompletedAt) && account.active <= 1 && !record.Failed
	predictedWork := p.nextWorkLocked(account, model)
	if workOK && !record.Failed && usage.GenerateEnabled(record.Generate) && !replay {
		p.seedLocked(account, model, work, at)
	}
	for _, name := range []string{"5h", "7d"} {
		prefix := "Anthropic-Ratelimit-Unified-" + name
		utilization, errUtil := strconv.ParseFloat(record.ResponseHeaders.Get(prefix+"-Utilization"), 64)
		seconds, errReset := strconv.ParseInt(record.ResponseHeaders.Get(prefix+"-Reset"), 10, 64)
		window := account.Windows[name]
		if errUtil != nil || math.IsNaN(utilization) || math.IsInf(utilization, 0) || utilization < 0 || errReset != nil || seconds <= 0 {
			delete(account.Windows, name)
			continue
		}
		reset := time.Unix(seconds, 0)
		if !at.Before(reset) {
			continue
		}
		if window == nil || !window.Reset.Equal(reset) || utilization < window.Utilization {
			window = &claudeQuotaWindow{Reset: reset}
			account.Windows[name] = window
		}
		delta := utilization - window.Utilization
		if serial && window.paired && workOK && usage.GenerateEnabled(record.Generate) && delta > 0 && predictedWork > 0 && len(window.Ratios) >= 3 {
			forecast := window.Utilization + claudeQuotaUpperRatio(window.Ratios)*(window.PendingWork+predictedWork)
			if !math.IsNaN(forecast) && !math.IsInf(forecast, 0) {
				p.evaluations = append(p.evaluations, ClaudeQuotaPredictionEvaluation{
					AccountKey: accountKey, Window: name, Model: canonicalClaudeQuotaModel(model), Estimator: "capped-p90-v1",
					ObservedAt: at, Reset: reset, BeforeUtilization: window.Utilization,
					PredictedUtilization: forecast, ActualUtilization: utilization,
					PredictedWork: predictedWork, ActualWork: work, CalibrationSamples: len(window.Ratios),
				})
				if len(p.evaluations) > claudeQuotaEvaluationLimit {
					p.evaluations = append([]ClaudeQuotaPredictionEvaluation(nil), p.evaluations[len(p.evaluations)-claudeQuotaEvaluationLimit:]...)
				}
			}
		}
		if serial && window.paired && workOK && delta > 0 {
			ratio := delta / (window.PendingWork + work)
			if !math.IsNaN(ratio) && !math.IsInf(ratio, 0) {
				window.Ratios = appendClaudeQuotaSample(window.Ratios, ratio, claudeQuotaCalibrationSamples)
			}
		}
		if delta > 0 || !serial || !window.paired || !workOK {
			window.PendingWork = 0
		} else {
			// Unchanged rounded headers can hide several small requests. Pair their
			// combined work with the next measurable current-response delta.
			window.PendingWork += work
		}
		window.Utilization, window.ObservedAt = utilization, at
		window.paired = serial && workOK
	}
	account.CompletedAt = at
	p.persistLocked()
}

func (p *claudeQuotaPredictor) nextWorkLocked(account *claudeQuotaAccount, model string) float64 {
	if account == nil {
		return 0
	}
	if model != "" {
		return claudeQuotaUpperSample(profileWork(account.Profiles[canonicalClaudeQuotaModel(model)]))
	}
	var samples []float64
	for _, profile := range account.Profiles {
		samples = append(samples, profile.Work...)
	}
	return claudeQuotaUpperSample(samples)
}

func profileWork(profile *claudeQuotaProfile) []float64 {
	if profile == nil {
		return nil
	}
	return profile.Work
}

func (p *claudeQuotaPredictor) reservedLocked(account *claudeQuotaAccount, model string, now time.Time) bool {
	if account == nil {
		return false
	}
	work := p.nextWorkLocked(account, model)
	for name, window := range account.Windows {
		duration := 5 * time.Hour
		if name == "7d" {
			duration = 7 * 24 * time.Hour
		}
		if window.ObservedAt.After(now) || !now.Before(window.Reset) || !now.Before(window.ObservedAt.Add(duration)) {
			continue
		}
		if window.Utilization >= .99 {
			return true
		}
		if work > 0 && len(window.Ratios) >= 3 && window.Utilization+claudeQuotaUpperRatio(window.Ratios)*(window.PendingWork+account.activeWork+work) >= .99 {
			return true
		}
	}
	return false
}

func (p *claudeQuotaPredictor) reserved(auth *Auth, model string, now time.Time) bool {
	if auth == nil || auth.AuthKind() != AuthKindOAuth || !strings.EqualFold(auth.Provider, "claude") {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.accounts[claudeQuotaAccountKey(auth)]
	p.discardSupersededWindowsLocked(account, auth)
	return p.reservedLocked(account, model, now)
}

// MarkResult can observe headers without a usage record. A newer changed signal
// invalidates calibration whose intervening workload is unknown.
func (p *claudeQuotaPredictor) discardSupersededWindowsLocked(account *claudeQuotaAccount, auth *Auth) {
	if account == nil {
		return
	}
	for name, window := range account.Windows {
		if !auth.Quota.ObservedAt.After(window.ObservedAt) {
			continue
		}
		prefix := "Anthropic-Ratelimit-Unified-" + name
		utilization, errUtil := strconv.ParseFloat(auth.Quota.Signals[prefix+"-Utilization"], 64)
		seconds, errReset := strconv.ParseInt(auth.Quota.Signals[prefix+"-Reset"], 10, 64)
		if errUtil == nil && utilization >= 0 && !math.IsInf(utilization, 0) && !math.IsNaN(utilization) &&
			(utilization != window.Utilization || (errReset == nil && seconds > 0 && !time.Unix(seconds, 0).Equal(window.Reset))) {
			delete(account.Windows, name)
		}
	}
}

func (p *claudeQuotaPredictor) begin(auth *Auth, model string, now time.Time) (func(), bool) {
	p.mu.Lock()
	account := p.accountLocked(claudeQuotaAccountKey(auth))
	p.discardSupersededWindowsLocked(account, auth)
	if p.reservedLocked(account, model, now) {
		p.mu.Unlock()
		return func() {}, false
	}
	if account == nil {
		p.mu.Unlock()
		return func() {}, true
	}
	work := p.nextWorkLocked(account, model)
	account.active++
	account.activeWork += work
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			account.active--
			account.activeWork = math.Max(0, account.activeWork-work)
			p.mu.Unlock()
		})
	}, true
}

// ObserveClaudeQuotaUsage consumes trusted executor records; historical imports use workload-only seeding.
func (m *Manager) ObserveClaudeQuotaUsage(record usage.Record) {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || !cfg.Claude.CacheKeepalive || !cfg.Claude.CacheKeepaliveReserveQuota || cfg.Home.Enabled || record.Provider != "claude" {
		return
	}
	auth, ok := m.GetByID(record.AuthID)
	if ok && auth.AuthKind() == AuthKindOAuth {
		m.claudeQuotaPrediction.observe(auth, record)
	}
}

// WithClaudeQuotaObservation attaches built-in observation without relying on a usage plugin.
func (m *Manager) WithClaudeQuotaObservation(ctx context.Context) context.Context {
	return usage.WithRecordObserver(ctx, func(_ context.Context, record usage.Record) { m.ObserveClaudeQuotaUsage(record) })
}

func (m *Manager) beginClaudeQuotaAttempt(auth *Auth, model string) (func(), bool) {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || !cfg.Claude.CacheKeepalive || !cfg.Claude.CacheKeepaliveReserveQuota || cfg.Home.Enabled || auth == nil || auth.AuthKind() != AuthKindOAuth || auth.Provider != "claude" {
		return func() {}, true
	}
	return m.claudeQuotaPrediction.begin(auth, model, time.Now())
}

// SetClaudeQuotaPredictionPersistence configures compact state beside persisted auths.
func (m *Manager) SetClaudeQuotaPredictionPersistence(dir string) error {
	return m.claudeQuotaPrediction.setPersistenceDir(dir)
}

// ClaudeQuotaPredictionEvaluations returns recent forecasts and later quota observations.
func (m *Manager) ClaudeQuotaPredictionEvaluations() []ClaudeQuotaPredictionEvaluation {
	p := m.claudeQuotaPrediction
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ClaudeQuotaPredictionEvaluation, len(p.evaluations))
	copy(out, p.evaluations)
	return out
}

func (p *claudeQuotaPredictor) persistLocked() {
	if err := p.saveLocked(); err != nil {
		log.WithError(err).Warn("claude quota prediction: persistence unavailable")
	}
}
