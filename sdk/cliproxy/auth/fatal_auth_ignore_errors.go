package auth

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

const (
	defaultFatalAuthIgnoreErrorsRepository = "router-for-me/CLIProxyAPI"
	fatalAuthIgnoreErrorsFilePath          = "sdk/cliproxy/auth/fatal-auth-ignore-errors.txt"
	fatalAuthIgnoreErrorsURLFormat         = "https://raw.githubusercontent.com/%s/main/%s"
	fatalAuthIgnoreErrorsFetchTimeout      = 15 * time.Second
	fatalAuthIgnorePlaceholderAnyValue     = "<任意值>"
)

var (
	//go:embed fatal-auth-ignore-errors.txt
	embeddedFatalAuthIgnoreErrors string

	fatalAuthIgnorePatternStore atomic.Value
)

type fatalAuthIgnorePattern struct {
	raw   string
	regex *regexp.Regexp
}

func init() {
	patterns, err := parseFatalAuthIgnorePatterns(strings.NewReader(embeddedFatalAuthIgnoreErrors))
	if err != nil {
		log.Errorf("failed to initialize embedded fatal auth ignore errors: %v", err)
		storeFatalAuthIgnorePatterns(nil)
		return
	}
	storeFatalAuthIgnorePatterns(patterns)
}

// InitializeFatalAuthIgnoreErrors 在服务启动时初始化 fatal auth 报错白名单。
// 默认先使用内置白名单，再尝试从当前镜像所属仓库拉取最新规则。
func InitializeFatalAuthIgnoreErrors(ctx context.Context, cfg *internalconfig.Config) {
	embeddedPatterns, err := parseFatalAuthIgnorePatterns(strings.NewReader(embeddedFatalAuthIgnoreErrors))
	if err != nil {
		log.Errorf("failed to parse embedded fatal auth ignore errors: %v", err)
		storeFatalAuthIgnorePatterns(nil)
	} else {
		storeFatalAuthIgnorePatterns(embeddedPatterns)
		log.Infof("loaded %d embedded fatal auth ignore error patterns", len(embeddedPatterns))
	}

	sourceURL := fatalAuthIgnoreErrorsURL(cfg)
	patterns, err := fetchFatalAuthIgnorePatterns(ctx, cfg, sourceURL)
	if err != nil {
		log.Warnf("failed to refresh fatal auth ignore errors from %s: %v", sourceURL, err)
		return
	}
	storeFatalAuthIgnorePatterns(patterns)
	log.Infof("loaded %d fatal auth ignore error patterns from %s", len(patterns), sourceURL)
}

func fatalAuthIgnoreErrorsURL(cfg *internalconfig.Config) string {
	if cfg != nil {
		if rawURL := strings.TrimSpace(cfg.FatalAuthIgnoreErrorsURL); rawURL != "" {
			return rawURL
		}
	}
	repository := strings.TrimSpace(buildinfo.Repository)
	if repository == "" {
		repository = defaultFatalAuthIgnoreErrorsRepository
	}
	repository = strings.TrimSpace(strings.TrimPrefix(repository, "https://github.com/"))
	repository = strings.Trim(strings.TrimPrefix(repository, "http://github.com/"), "/")
	if repository == "" {
		repository = defaultFatalAuthIgnoreErrorsRepository
	}
	return fmt.Sprintf(fatalAuthIgnoreErrorsURLFormat, repository, fatalAuthIgnoreErrorsFilePath)
}

func fetchFatalAuthIgnorePatterns(ctx context.Context, cfg *internalconfig.Config, rawURL string) ([]fatalAuthIgnorePattern, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("fatal auth ignore errors url is empty")
	}

	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, fatalAuthIgnoreErrorsFetchTimeout)
	defer cancel()

	client := &http.Client{}
	if cfg != nil {
		util.SetProxy(&sdkconfig.SDKConfig{ProxyURL: cfg.ProxyURL}, client)
	}

	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "CLIProxyAPI/"+buildinfo.Version)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	patterns, err := parseFatalAuthIgnorePatterns(resp.Body)
	if err != nil {
		return nil, err
	}
	return patterns, nil
}

func parseFatalAuthIgnorePatterns(reader io.Reader) ([]fatalAuthIgnorePattern, error) {
	if reader == nil {
		return nil, nil
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024), 1024*1024)

	patterns := make([]fatalAuthIgnorePattern, 0)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		regex, err := compileFatalAuthIgnorePattern(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		patterns = append(patterns, fatalAuthIgnorePattern{
			raw:   line,
			regex: regex,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return patterns, nil
}

func compileFatalAuthIgnorePattern(raw string) (*regexp.Regexp, error) {
	normalized := strings.TrimSpace(raw)
	if normalized == "" {
		return nil, fmt.Errorf("empty fatal auth ignore pattern")
	}

	const anyValueToken = "<<__FATAL_AUTH_IGNORE_ANY_VALUE__>>"
	const wildcardToken = "<<__FATAL_AUTH_IGNORE_WILDCARD__>>"

	pattern := strings.ReplaceAll(normalized, fatalAuthIgnorePlaceholderAnyValue, anyValueToken)
	pattern = strings.ReplaceAll(pattern, "*", wildcardToken)
	pattern = regexp.QuoteMeta(pattern)
	pattern = strings.ReplaceAll(pattern, regexp.QuoteMeta(anyValueToken), ".*")
	pattern = strings.ReplaceAll(pattern, regexp.QuoteMeta(wildcardToken), ".*")

	return regexp.Compile("(?i)^" + pattern + "$")
}

func shouldIgnoreAuthErrorByWhitelist(err *Error) bool {
	if err == nil {
		return false
	}
	patterns := loadFatalAuthIgnorePatterns()
	if len(patterns) == 0 {
		return false
	}

	candidates := make([]string, 0, 3)
	if message := strings.TrimSpace(err.Message); message != "" {
		candidates = append(candidates, message)
	}
	if text := strings.TrimSpace(err.Error()); text != "" && text != err.Message {
		candidates = append(candidates, text)
	}
	if code := strings.TrimSpace(err.Code); code != "" {
		candidates = append(candidates, code)
	}

	for _, candidate := range candidates {
		for _, pattern := range patterns {
			if pattern.regex != nil && pattern.regex.MatchString(candidate) {
				return true
			}
		}
	}
	return false
}

func loadFatalAuthIgnorePatterns() []fatalAuthIgnorePattern {
	patterns, _ := fatalAuthIgnorePatternStore.Load().([]fatalAuthIgnorePattern)
	return patterns
}

func storeFatalAuthIgnorePatterns(patterns []fatalAuthIgnorePattern) {
	if patterns == nil {
		fatalAuthIgnorePatternStore.Store([]fatalAuthIgnorePattern(nil))
		return
	}
	cloned := make([]fatalAuthIgnorePattern, len(patterns))
	copy(cloned, patterns)
	fatalAuthIgnorePatternStore.Store(cloned)
}
