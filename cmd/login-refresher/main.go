package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github-login-refresher/internal/login"
	"github-login-refresher/internal/refreshapi"
)

func main() {
	managerURL := flag.String("manager-url", envOr("MANAGER_EXTERNAL_URL", ""), "external Manager URL, for example http://your-manager.example.com")
	jobID := flag.String("job-id", envOr("LOGIN_REFRESH_JOB_ID", ""), "login refresh job id")
	groupIndex := flag.Int("group-index", envIntOr("LOGIN_REFRESH_GROUP_INDEX", 0), "login refresh group index")
	token := flag.String("token", envOr("MANAGER_EXTERNAL_TOKEN", ""), "external Manager bearer token")
	chromePath := flag.String("chrome", envOr("LOGIN_CHROME", ""), "Chrome/Edge executable path")
	proxy := flag.String("proxy", envOr("LOGIN_PROXY", ""), "optional login proxy")
	concurrency := flag.Int("concurrency", envIntOr("LOGIN_REFRESH_CONCURRENCY", 0), "browser login concurrency; default uses Manager group setting")
	timeout := flag.Duration("timeout", envDurationOr("LOGIN_TIMEOUT", 5*time.Minute), "single login timeout")
	headless := flag.Bool("headless", envBoolOr("LOGIN_HEADLESS", true), "run browser in headless mode")
	flag.Parse()

	if strings.TrimSpace(*managerURL) == "" || strings.TrimSpace(*jobID) == "" || strings.TrimSpace(*token) == "" {
		log.Fatal("manager-url, job-id and token are required")
	}
	target, err := login.LoadTargetConfigFromEnv()
	if err != nil {
		log.Fatalf("load target config: %v", err)
	}

	client := &http.Client{Timeout: 45 * time.Second}
	ctx := context.Background()
	group, err := fetchGroup(ctx, client, *managerURL, *jobID, *groupIndex, *token)
	if err != nil {
		log.Fatalf("fetch group: %v", err)
	}
	workers := *concurrency
	if workers <= 0 {
		workers = group.BrowsersPerRunner
	}
	if workers <= 0 {
		workers = 1
	}

	loginRunner := login.NewRunner(login.Config{
		Proxy:        *proxy,
		ChromePath:   *chromePath,
		Timeout:      *timeout,
		QueueTimeout: time.Duration(len(group.Accounts)+workers) * *timeout,
		Headless:     *headless,
		Workers:      workers,
		Target:       target,
		Logger: func(event login.LogEvent) {
			log.Printf("%s %s %s %s", event.Level, event.Type, event.CustomerCode, event.Message)
		},
	})
	results := refreshAccounts(ctx, loginRunner, group.Accounts, workers)

	resultFields := refreshapi.CredentialFieldNames{
		Primary: target.PrimaryCookieName,
		Session: target.SessionCookieName,
	}
	if err := postResults(ctx, client, *managerURL, *jobID, *groupIndex, *token, results, resultFields); err != nil {
		log.Fatalf("post results: %v", err)
	}
	status := refreshapi.LoginRefreshGroupStatusCompleted
	message := "completed"
	for _, result := range results {
		if !result.Success {
			status = refreshapi.LoginRefreshGroupStatusFailed
			message = "completed with failures"
			break
		}
	}
	if err := completeGroup(ctx, client, *managerURL, *jobID, *groupIndex, *token, status, message); err != nil {
		log.Fatalf("complete group: %v", err)
	}
	log.Printf("group %d complete: %d accounts", *groupIndex, len(results))
}

func refreshAccounts(ctx context.Context, loginRunner *login.Runner, accounts []refreshapi.LoginRefreshJobAccount, workers int) []refreshapi.LoginRefreshAccountResult {
	if workers <= 0 {
		workers = 1
	}
	results := make([]refreshapi.LoginRefreshAccountResult, 0, len(accounts))
	resultsByIndex := make([]refreshapi.LoginRefreshAccountResult, len(accounts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for index, account := range accounts {
		index := index
		account := account
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			code := refreshapi.NormalizeCustomerCode(account.CustomerCode)
			log.Printf("refreshing %s", code)
			result, err := loginRunner.Login(ctx, code, account.Password)
			if err != nil {
				log.Printf("refresh %s failed: %v", code, err)
				resultsByIndex[index] = refreshapi.LoginRefreshAccountResult{
					CustomerCode: code,
					Success:      false,
					Message:      err.Error(),
				}
				return
			}
			if result.CustomerCode == "" {
				result.CustomerCode = code
			}
			voucher := result.CanUseVoucher
			resultsByIndex[index] = refreshapi.LoginRefreshAccountResult{
				CustomerCode:      result.CustomerCode,
				Success:           true,
				TGC:               result.TGC,
				PrimaryCredential: result.PrimaryCredential,
				SessionCredential: result.SessionCredential,
				MobileAccessToken: result.MobileAccessToken,
				CanUseVoucher:     &voucher,
				Message:           "ok",
			}
		}()
	}
	wg.Wait()

	for _, result := range resultsByIndex {
		if result.CustomerCode != "" {
			results = append(results, result)
		}
	}
	return results
}

func fetchGroup(ctx context.Context, client *http.Client, baseURL, jobID string, groupIndex int, token string) (refreshapi.LoginRefreshGroupAccountsResponse, error) {
	var out refreshapi.LoginRefreshGroupAccountsResponse
	url := fmt.Sprintf("%s/api/v1/external/login-refresh-jobs/%s/groups/%d", strings.TrimRight(baseURL, "/"), jobID, groupIndex)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	body, status, err := doJSON(client, req)
	if err != nil {
		return out, err
	}
	if status < 200 || status >= 300 {
		return out, fmt.Errorf("manager returned %d: %s", status, string(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, err
	}
	return out, nil
}

func postResults(ctx context.Context, client *http.Client, baseURL, jobID string, groupIndex int, token string, results []refreshapi.LoginRefreshAccountResult, fields refreshapi.CredentialFieldNames) error {
	body, err := json.Marshal(refreshapi.BuildLoginRefreshResultRequest(results, fields))
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/v1/external/login-refresh-jobs/%s/groups/%d/results", strings.TrimRight(baseURL, "/"), jobID, groupIndex)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	respBody, status, err := doJSON(client, req)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("manager returned %d: %s", status, string(respBody))
	}
	return nil
}

func completeGroup(ctx context.Context, client *http.Client, baseURL, jobID string, groupIndex int, token, status, message string) error {
	body, err := json.Marshal(refreshapi.LoginRefreshGroupCompleteRequest{Status: status, Message: message})
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/v1/external/login-refresh-jobs/%s/groups/%d/complete", strings.TrimRight(baseURL, "/"), jobID, groupIndex)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	respBody, statusCode, err := doJSON(client, req)
	if err != nil {
		return err
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("manager returned %d: %s", statusCode, string(respBody))
	}
	return nil
}

func doJSON(client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	value := envOr(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envIntOr(key string, fallback int) int {
	value := envOr(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBoolOr(key string, fallback bool) bool {
	value := envOr(key, "")
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
