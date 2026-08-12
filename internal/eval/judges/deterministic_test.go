package judges

import "testing"

// Test contains_any check
func TestContainsAny(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		patterns []string
		want     bool
	}{
		{
			name:     "positive_single_match",
			text:     "This is a test string with match",
			patterns: []string{"match"},
			want:     true,
		},
		{
			name:     "positive_multiple_patterns",
			text:     "The algorithm uses recursion",
			patterns: []string{"loop", "recursion", "iteration"},
			want:     true,
		},
		{
			name:     "negative_no_match",
			text:     "This is a test string",
			patterns: []string{"match", "found", "located"},
			want:     false,
		},
		{
			name:     "negative_empty_patterns",
			text:     "This is a test string",
			patterns: []string{},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainsAny(tt.text, tt.patterns)
			if got != tt.want {
				t.Errorf("ContainsAny() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test not_contains check
func TestNotContains(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		pattern string
		want    bool
	}{
		{
			name:    "positive_not_present",
			text:    "This is a test string",
			pattern: "missing",
			want:    true,
		},
		{
			name:    "negative_present",
			text:    "This is a test string with pattern",
			pattern: "pattern",
			want:    false,
		},
		{
			name:    "positive_empty_text",
			text:    "",
			pattern: "something",
			want:    true,
		},
		{
			name:    "positive_case_sensitive",
			text:    "This is a test",
			pattern: "TEST",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NotContains(tt.text, tt.pattern)
			if got != tt.want {
				t.Errorf("NotContains() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test regex_match check
func TestRegexMatch(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		pattern string
		want    bool
		wantErr bool
	}{
		{
			name:    "positive_match",
			text:    "The year is 2025",
			pattern: `\d{4}`,
			want:    true,
			wantErr: false,
		},
		{
			name:    "negative_no_match",
			text:    "No numbers here",
			pattern: `\d+`,
			want:    false,
			wantErr: false,
		},
		{
			name:    "positive_complex_pattern",
			text:    "Email: user@example.com",
			pattern: `\w+@\w+\.\w+`,
			want:    true,
			wantErr: false,
		},
		{
			name:    "negative_invalid_regex",
			text:    "some text",
			pattern: `[invalid(`,
			want:    false,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RegexMatch(tt.text, tt.pattern)
			if (err != nil) != tt.wantErr {
				t.Errorf("RegexMatch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("RegexMatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test regex_count check
func TestRegexCount(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		pattern   string
		threshold int
		want      bool
		wantErr   bool
	}{
		{
			name:      "positive_meets_threshold",
			text:      "apple, apple, banana, apple",
			pattern:   `apple`,
			threshold: 3,
			want:      true,
			wantErr:   false,
		},
		{
			name:      "negative_below_threshold",
			text:      "apple, banana",
			pattern:   `apple`,
			threshold: 3,
			want:      false,
			wantErr:   false,
		},
		{
			name:      "positive_equals_threshold",
			text:      "apple, apple, apple",
			pattern:   `apple`,
			threshold: 3,
			want:      true,
			wantErr:   false,
		},
		{
			name:      "positive_regex_pattern",
			text:      "test123 and test456 and test789",
			pattern:   `test\d+`,
			threshold: 3,
			want:      true,
			wantErr:   false,
		},
		{
			name:      "negative_no_matches",
			text:      "no matches here",
			pattern:   `\?`,
			threshold: 1,
			want:      false,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RegexCount(tt.text, tt.pattern, tt.threshold)
			if (err != nil) != tt.wantErr {
				t.Errorf("RegexCount() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("RegexCount() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test tool_called check
func TestToolCalled(t *testing.T) {
	tests := []struct {
		name      string
		toolName  string
		toolCalls []string
		want      bool
	}{
		{
			name:      "positive_tool_present",
			toolName:  "grep",
			toolCalls: []string{"bash", "grep", "sed"},
			want:      true,
		},
		{
			name:      "negative_tool_absent",
			toolName:  "awk",
			toolCalls: []string{"bash", "grep", "sed"},
			want:      false,
		},
		{
			name:      "positive_single_tool",
			toolName:  "curl",
			toolCalls: []string{"curl"},
			want:      true,
		},
		{
			name:      "negative_empty_list",
			toolName:  "python",
			toolCalls: []string{},
			want:      false,
		},
		{
			name:      "positive_case_sensitive",
			toolName:  "Python",
			toolCalls: []string{"python", "bash"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToolCalled(tt.toolName, tt.toolCalls)
			if got != tt.want {
				t.Errorf("ToolCalled() = %v, want %v", got, tt.want)
			}
		})
	}
}
