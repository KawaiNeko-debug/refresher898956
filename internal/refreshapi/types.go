package refreshapi

import "strings"

const (
	LoginRefreshGroupStatusCompleted = "completed"
	LoginRefreshGroupStatusFailed    = "failed"
)

type LoginRefreshJobAccount struct {
	CustomerCode string `json:"customerCode"`
	Phone        string `json:"phone,omitempty"`
	Password     string `json:"password,omitempty"`
}

type LoginRefreshGroupAccountsResponse struct {
	JobID             string                   `json:"jobId"`
	GroupIndex        int                      `json:"groupIndex"`
	BrowsersPerRunner int                      `json:"browsersPerRunner"`
	Accounts          []LoginRefreshJobAccount `json:"accounts"`
}

type LoginRefreshResultRequest struct {
	Results []LoginRefreshAccountResult `json:"results"`
}

type LoginRefreshAccountResult struct {
	CustomerCode      string `json:"customerCode"`
	Success           bool   `json:"success"`
	TGC               string `json:"tgc,omitempty"`
	PrimaryCredential string `json:"primaryCredential,omitempty"`
	SessionCredential string `json:"sessionCredential,omitempty"`
	MobileAccessToken string `json:"mobileAccessToken,omitempty"`
	CanUseVoucher     *int   `json:"canUseVoucher,omitempty"`
	Message           string `json:"message,omitempty"`
}

type CredentialFieldNames struct {
	Primary string
	Session string
}

type LoginRefreshGroupCompleteRequest struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func NormalizeCustomerCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func BuildLoginRefreshResultRequest(results []LoginRefreshAccountResult, fields CredentialFieldNames) map[string]any {
	items := make([]map[string]any, 0, len(results))
	for _, result := range results {
		item := map[string]any{
			"customerCode": result.CustomerCode,
			"success":      result.Success,
		}
		if result.TGC != "" {
			item["tgc"] = result.TGC
		}
		if result.PrimaryCredential != "" && fields.Primary != "" {
			item[fields.Primary] = result.PrimaryCredential
		}
		if result.SessionCredential != "" && fields.Session != "" {
			item[fields.Session] = result.SessionCredential
		}
		if result.MobileAccessToken != "" {
			item["mobileAccessToken"] = result.MobileAccessToken
		}
		if result.CanUseVoucher != nil {
			item["canUseVoucher"] = *result.CanUseVoucher
		}
		if result.Message != "" {
			item["message"] = result.Message
		}
		items = append(items, item)
	}
	return map[string]any{"results": items}
}
