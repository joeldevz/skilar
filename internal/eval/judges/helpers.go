package judges

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

func ContainsAny(text string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern != "" && strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

func ContainsAll(text string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, pattern := range patterns {
		if pattern == "" || !strings.Contains(text, pattern) {
			return false
		}
	}
	return true
}

func NotContains(text, pattern string) bool {
	return pattern == "" || !strings.Contains(text, pattern)
}

func RegexMatch(text, pattern string) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(text), nil
}

func RegexCount(text, pattern string, threshold int) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return len(re.FindAllString(text, -1)) >= threshold, nil
}

func ToolCalled(toolName string, toolCalls []string) bool {
	for _, tool := range toolCalls {
		if tool == toolName {
			return true
		}
	}
	return false
}

func ContentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
