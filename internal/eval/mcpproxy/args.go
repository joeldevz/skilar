package mcpproxy

import (
	"fmt"
	"strings"
)

// ParseArgs parses the intentionally hidden __mcp-proxy command. It uses a
// small fixed grammar so child argv and environment values are never emitted
// by flag-package diagnostics.
func ParseArgs(args []string) (Config, error) {
	var config Config
	config.Environment = make(map[string]string)
	for index := 0; index < len(args); {
		if args[index] == "--" {
			config.Command = append([]string(nil), args[index+1:]...)
			return validateDeclaredConfig(config)
		}
		if index+1 >= len(args) {
			return Config{}, fmt.Errorf("%w: incomplete arguments", ErrInvalidProxyConfig)
		}
		name, value := args[index], args[index+1]
		switch name {
		case "--mcp-name":
			if config.MCPName != "" {
				return Config{}, fmt.Errorf("%w: duplicate MCP name", ErrInvalidProxyConfig)
			}
			config.MCPName = value
		case "--tool":
			config.ExpectedTools = append(config.ExpectedTools, value)
		case "--env":
			key, environmentValue, ok := strings.Cut(value, "=")
			if !ok || key == "" {
				return Config{}, fmt.Errorf("%w: invalid environment argument", ErrInvalidProxyConfig)
			}
			if _, duplicate := config.Environment[key]; duplicate {
				return Config{}, fmt.Errorf("%w: duplicate environment argument", ErrInvalidProxyConfig)
			}
			config.Environment[key] = environmentValue
		default:
			return Config{}, fmt.Errorf("%w: unknown argument", ErrInvalidProxyConfig)
		}
		index += 2
	}
	return Config{}, fmt.Errorf("%w: missing child command separator", ErrInvalidProxyConfig)
}
