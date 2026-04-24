package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ccVersionInBillingRe matches the semver part of cc_version (X.Y.Z), preserving
// the trailing message-derived suffix (e.g. ".c02") if present.
var ccVersionInBillingRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+`)

var extractCCVersionRe = regexp.MustCompile(`cc_version=(\d+\.\d+\.\d+)`)
var replaceCchRe = regexp.MustCompile(`\s*cch=[^;\"']+;?`)
var replaceCcVersionRe = regexp.MustCompile(`cc_version=\d+\.\d+\.\d+(?:\.[a-fA-F0-9]+)?`)

const claudeFingerprintSalt = "59cf53e54c78"

// syncBillingHeaderVersion rewrites cc_version in x-anthropic-billing-header
// system text blocks to match the version extracted from userAgent.
// Only touches system array blocks whose text starts with "x-anthropic-billing-header".
func syncBillingHeaderVersion(body []byte, userAgent string) []byte {
	version := ExtractCLIVersion(userAgent)
	if version == "" {
		return body
	}

	systemResult := gjson.GetBytes(body, "system")
	if !systemResult.Exists() || !systemResult.IsArray() {
		return body
	}

	replacement := "cc_version=" + version
	idx := 0
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text := item.Get("text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			newText := ccVersionInBillingRe.ReplaceAllString(text.String(), replacement)
			if newText != text.String() {
				if updated, err := sjson.SetBytes(body, fmt.Sprintf("system.%d.text", idx), newText); err == nil {
					body = updated
				}
			}
		}
		idx++
		return true
	})

	return body
}

// signBillingHeaderCCH computes the true SHA256-based fingerprint for the request
// and updates the cc_version suffix. It also strips the detectable cch=00000 placeholder.
func signBillingHeaderCCH(body []byte) []byte {
	systemResult := gjson.GetBytes(body, "system")
	if !systemResult.Exists() || !systemResult.IsArray() {
		return body
	}

	var versionStr string
	hasBilling := false
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text := item.Get("text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			hasBilling = true
			matches := extractCCVersionRe.FindStringSubmatch(text.String())
			if len(matches) == 2 {
				versionStr = matches[1]
			}
		}
		return !hasBilling // break if found
	})

	if !hasBilling {
		return body
	}

	msgText := extractFirstUserMessageTextForFingerprint(body)
	fp := computeClaudeFingerprint(msgText, versionStr)

	idx := 0
	systemResult.ForEach(func(_, item gjson.Result) bool {
		text := item.Get("text")
		if text.Exists() && text.Type == gjson.String &&
			strings.HasPrefix(text.String(), "x-anthropic-billing-header") {
			
			newText := text.String()
			
			// 1. Remove cch=... placeholder since true Claude Code does not use it
			newText = replaceCchRe.ReplaceAllString(newText, "")
			
			// 2. Rewrite cc_version to include the true SHA256 fingerprint suffix
			if versionStr != "" && fp != "" {
				replacement := "cc_version=" + versionStr + "." + fp
				newText = replaceCcVersionRe.ReplaceAllString(newText, replacement)
			}
			
			if newText != text.String() {
				if updated, err := sjson.SetBytes(body, fmt.Sprintf("system.%d.text", idx), newText); err == nil {
					body = updated
				}
			}
		}
		idx++
		return true
	})
	return body
}

func extractFirstUserMessageTextForFingerprint(body []byte) string {
	var text string
	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			if msg.Get("role").String() == "user" {
				content := msg.Get("content")
				if content.Type == gjson.String {
					text = content.String()
				} else if content.IsArray() {
					content.ForEach(func(_, block gjson.Result) bool {
						if block.Get("type").String() == "text" {
							text = block.Get("text").String()
							return false
						}
						return true
					})
				}
				return false // Break after first user message
			}
			return true
		})
	}
	return text
}

func computeClaudeFingerprint(message, version string) string {
	rn := []rune(message)
	p4 := runeAtOrZero(rn, 4)
	p7 := runeAtOrZero(rn, 7)
	p20 := runeAtOrZero(rn, 20)

	h := sha256.New()
	h.Write([]byte(claudeFingerprintSalt + p4 + p7 + p20 + version))
	hashHex := hex.EncodeToString(h.Sum(nil))
	if len(hashHex) >= 3 {
		return hashHex[:3]
	}
	return ""
}

func runeAtOrZero(rn []rune, idx int) string {
	if idx < len(rn) {
		return string(rn[idx])
	}
	return "0"
}
