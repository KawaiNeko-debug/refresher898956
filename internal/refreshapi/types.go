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
	ProdJLCCASSID     string `json:"PROD-JLC-CAS-SID,omitempty"`
	JLCGroupSessionID string `json:"JLCGROUP_SESSIONID,omitempty"`
	MobileAccessToken string `json:"mobileAccessToken,omitempty"`
	CanUseVoucher     *int   `json:"canUseVoucher,omitempty"`
	Message           string `json:"message,omitempty"`
}

type LoginRefreshGroupCompleteRequest struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

func NormalizeCustomerCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}
