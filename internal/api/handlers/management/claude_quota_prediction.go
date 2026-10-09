package management

import (
	"math"
	"net/http"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type claudeQuotaPredictionError struct {
	Count               int     `json:"count"`
	MeanAbsoluteErrorPP float64 `json:"mean_absolute_error_pp"`
	MeanSignedErrorPP   float64 `json:"mean_signed_error_pp"`
}

// GetClaudeQuotaPrediction reports measured one-request forecast errors in percentage points.
func (h *Handler) GetClaudeQuotaPrediction(c *gin.Context) {
	samples := []coreauth.ClaudeQuotaPredictionEvaluation{}
	if h.authManager != nil {
		samples = h.authManager.ClaudeQuotaPredictionEvaluations()
	}
	summary := map[string]*claudeQuotaPredictionError{
		"5h": {},
		"7d": {},
	}
	for _, sample := range samples {
		item := summary[sample.Window]
		if item == nil {
			continue
		}
		errorPP := 100 * (sample.PredictedUtilization - sample.ActualUtilization)
		item.Count++
		item.MeanAbsoluteErrorPP += math.Abs(errorPP)
		item.MeanSignedErrorPP += errorPP
	}
	for _, item := range summary {
		if item.Count > 0 {
			item.MeanAbsoluteErrorPP /= float64(item.Count)
			item.MeanSignedErrorPP /= float64(item.Count)
		}
	}
	c.JSON(http.StatusOK, gin.H{"samples": samples, "summary": summary})
}
