package service

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestBuildClaudeMimicDebugLineContainsNoUntrustedValues(t *testing.T) {
	requestURL, err := url.Parse("https://url-user:url-password@example.com/private/path?token=url-query-secret")
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{
		URL: requestURL,
		Header: http.Header{
			"User-Agent":       {"header-user-agent-secret"},
			"Anthropic-Beta":   {"header-beta-secret"},
			"X-Stainless-Lang": {"header-stainless-secret"},
			"Authorization":    {"Bearer header-token-secret"},
		},
	}
	body := []byte(`{"metadata":{"user_id":"metadata-user-secret"},"system":"system-prompt-secret"}`)
	account := &Account{ID: 731, Name: "account-name-secret"}
	line := buildClaudeMimicDebugLine(req, body, account, "token-type-secret", true)

	for _, secret := range []string{
		"url-user", "url-password", "private/path", "url-query-secret",
		"header-user-agent-secret", "header-beta-secret", "header-stainless-secret",
		"header-token-secret", "metadata-user-secret", "system-prompt-secret",
		"account-name-secret", "token-type-secret",
	} {
		if strings.Contains(line, secret) {
			t.Errorf("debug line leaked %q: %s", secret, line)
		}
	}
	for _, safe := range []string{
		"account=731", "tokenType=other", "mimic=true",
		"metadata.present=true", "metadata.type=object", "metadata.user_id.present=true",
		"metadata.user_id.type=string", "system.present=true", "system.type=string",
		"user-agent=present", "anthropic-beta=present", "x-stainless-lang=present", "authorization=present",
	} {
		if !strings.Contains(line, safe) {
			t.Errorf("debug line missing safe structural label %q: %s", safe, line)
		}
	}
}

func TestBuildClaudeMimicDebugLineUsesFixedLabelsForTokenTypeAndAbsentFields(t *testing.T) {
	line := buildClaudeMimicDebugLine(&http.Request{Header: make(http.Header)}, nil, nil, "oauth", false)
	for _, safe := range []string{
		"account=0", "tokenType=oauth", "mimic=false",
		"metadata.present=false", "metadata.type=absent", "metadata.user_id.present=false",
		"metadata.user_id.type=absent", "system.present=false", "system.type=absent",
	} {
		if !strings.Contains(line, safe) {
			t.Errorf("debug line missing safe structural label %q: %s", safe, line)
		}
	}
}
