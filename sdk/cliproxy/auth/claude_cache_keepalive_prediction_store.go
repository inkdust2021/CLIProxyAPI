package auth

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const claudeQuotaPredictionFile = ".claude-cache-quota.json"

type claudeQuotaPersistedState struct {
	Version     int                               `json:"version"`
	Accounts    map[string]*claudeQuotaAccount    `json:"accounts"`
	Evaluations []ClaudeQuotaPredictionEvaluation `json:"evaluations,omitempty"`
}

var errClaudeQuotaPredictionState = errors.New("claude quota prediction state unavailable or invalid")

func (p *claudeQuotaPredictor) setPersistenceDir(dir string) error {
	if dir != "" {
		resolved, err := filepath.Abs(dir)
		if err != nil {
			return errClaudeQuotaPredictionState
		}
		dir = resolved
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dir == dir {
		if p.blocked {
			return errClaudeQuotaPredictionState
		}
		return nil
	}
	p.dir, p.blocked = dir, false
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		p.blocked = true
		return errClaudeQuotaPredictionState
	}
	path := filepath.Join(dir, claudeQuotaPredictionFile)
	info, errInfo := os.Stat(path)
	if errors.Is(errInfo, os.ErrNotExist) && strings.HasSuffix(dir, "-state") {
		legacyPath := filepath.Join(strings.TrimSuffix(dir, "-state"), claudeQuotaPredictionFile)
		if legacyInfo, legacyErr := os.Stat(legacyPath); legacyErr == nil {
			path, info, errInfo = legacyPath, legacyInfo, nil
		}
	}
	if errors.Is(errInfo, os.ErrNotExist) {
		return p.saveLocked()
	}
	if errInfo != nil || info.Size() > 1<<20 {
		p.blocked = true
		return errClaudeQuotaPredictionState
	}
	data, errRead := os.ReadFile(path)
	var state claudeQuotaPersistedState
	if errRead != nil || json.Unmarshal(data, &state) != nil || !validClaudeQuotaState(state) {
		p.blocked = true
		p.accounts = make(map[string]*claudeQuotaAccount)
		p.evaluations = nil
		return errClaudeQuotaPredictionState
	}
	for _, account := range state.Accounts {
		for _, window := range account.Windows {
			window.paired = false
		}
	}
	p.accounts = state.Accounts
	p.evaluations = state.Evaluations
	if path != filepath.Join(dir, claudeQuotaPredictionFile) {
		if errMigrate := p.saveLocked(); errMigrate != nil {
			p.blocked = true
			return errClaudeQuotaPredictionState
		}
		_ = os.Remove(path)
	}
	return nil
}

func validClaudeQuotaState(state claudeQuotaPersistedState) bool {
	if state.Version != 1 || state.Accounts == nil || len(state.Accounts) > claudeQuotaMaxAccounts || len(state.Evaluations) > claudeQuotaEvaluationLimit {
		return false
	}
	finiteNonnegative := func(value float64) bool {
		return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
	}
	for _, evaluation := range state.Evaluations {
		key, err := hex.DecodeString(evaluation.AccountKey)
		if err != nil || len(key) != 32 || (evaluation.Window != "5h" && evaluation.Window != "7d") || len(evaluation.Model) > 128 ||
			evaluation.ObservedAt.IsZero() || !evaluation.Reset.After(evaluation.ObservedAt) ||
			!finiteNonnegative(evaluation.BeforeUtilization) || !finiteNonnegative(evaluation.PredictedUtilization) ||
			!finiteNonnegative(evaluation.ActualUtilization) || evaluation.ActualUtilization <= evaluation.BeforeUtilization ||
			!finiteNonnegative(evaluation.PredictedWork) || evaluation.PredictedWork == 0 ||
			!finiteNonnegative(evaluation.ActualWork) || evaluation.ActualWork == 0 ||
			evaluation.CalibrationSamples < 3 || evaluation.CalibrationSamples > claudeQuotaCalibrationSamples {
			return false
		}
	}
	validSamples := func(samples []float64, limit int) bool {
		if len(samples) > limit {
			return false
		}
		for _, sample := range samples {
			if sample <= 0 || math.IsNaN(sample) || math.IsInf(sample, 0) {
				return false
			}
		}
		return true
	}
	for key, account := range state.Accounts {
		decoded, err := hex.DecodeString(key)
		if err != nil || len(decoded) != 32 || account == nil || account.Profiles == nil || account.Windows == nil || len(account.Profiles) > claudeQuotaMaxModels || len(account.Windows) > 2 {
			return false
		}
		for model, profile := range account.Profiles {
			if len(model) > 128 || profile == nil || !validSamples(profile.Work, claudeQuotaProfileSamples) {
				return false
			}
		}
		for name, window := range account.Windows {
			if (name != "5h" && name != "7d") || window == nil || window.Utilization < 0 || math.IsNaN(window.Utilization) || math.IsInf(window.Utilization, 0) || window.PendingWork < 0 || math.IsNaN(window.PendingWork) || math.IsInf(window.PendingWork, 0) || window.ObservedAt.IsZero() || !window.Reset.After(window.ObservedAt) || !validSamples(window.Ratios, claudeQuotaCalibrationSamples) {
				return false
			}
		}
	}
	return true
}

func (p *claudeQuotaPredictor) saveLocked() error {
	if p.dir == "" || p.blocked {
		return nil
	}
	data, errMarshal := json.Marshal(claudeQuotaPersistedState{Version: 1, Accounts: p.accounts, Evaluations: p.evaluations})
	if errMarshal != nil {
		return errClaudeQuotaPredictionState
	}
	temp, errCreate := os.CreateTemp(p.dir, ".claude-quota-*")
	if errCreate != nil {
		return errClaudeQuotaPredictionState
	}
	path := temp.Name()
	defer func() { _ = os.Remove(path) }()
	if errWrite := temp.Chmod(0600); errWrite != nil {
		_ = temp.Close()
		return errClaudeQuotaPredictionState
	}
	if _, errWrite := temp.Write(data); errWrite != nil {
		_ = temp.Close()
		return errClaudeQuotaPredictionState
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return errClaudeQuotaPredictionState
	}
	if errClose := temp.Close(); errClose != nil {
		return errClaudeQuotaPredictionState
	}
	if errRename := os.Rename(path, filepath.Join(p.dir, claudeQuotaPredictionFile)); errRename != nil {
		return errClaudeQuotaPredictionState
	}
	return nil
}
