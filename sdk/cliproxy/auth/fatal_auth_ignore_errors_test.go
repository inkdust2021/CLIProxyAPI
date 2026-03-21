package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestCompileFatalAuthIgnorePattern(t *testing.T) {
	pattern, err := compileFatalAuthIgnorePattern(`level "<任意值>" not supported`)
	if err != nil {
		t.Fatalf("compile fatal auth ignore pattern: %v", err)
	}

	if !pattern.MatchString(`level "verbose" not supported`) {
		t.Fatalf("expected pattern to match variable level content")
	}
	if pattern.MatchString(`stream error: upstream timeout`) {
		t.Fatalf("expected pattern not to match unrelated message")
	}
}

func TestFetchFatalAuthIgnorePatterns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "# 注释")
		fmt.Fprintln(w, `level "<任意值>" not supported`)
	}))
	defer server.Close()

	patterns, err := fetchFatalAuthIgnorePatterns(context.Background(), &internalconfig.Config{}, server.URL)
	if err != nil {
		t.Fatalf("fetch fatal auth ignore patterns: %v", err)
	}
	if len(patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(patterns))
	}
	if !patterns[0].regex.MatchString(`level "debug" not supported`) {
		t.Fatalf("expected fetched pattern to match runtime error")
	}
}

func TestFatalAuthIgnoreErrorsURLUsesOverride(t *testing.T) {
	cfg := &internalconfig.Config{
		SDKConfig: internalconfig.SDKConfig{
			FatalAuthIgnoreErrorsURL: "https://example.com/custom.txt",
		},
	}
	if got := fatalAuthIgnoreErrorsURL(cfg); got != "https://example.com/custom.txt" {
		t.Fatalf("unexpected override url: %s", got)
	}
}

func TestParseFatalAuthIgnorePatternsSkipsCommentsAndBlankLines(t *testing.T) {
	patterns, err := parseFatalAuthIgnorePatterns(strings.NewReader("\n# 注释\nlevel \"<任意值>\" not supported\n"))
	if err != nil {
		t.Fatalf("parse fatal auth ignore patterns: %v", err)
	}
	if len(patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(patterns))
	}
}
