package lifecycle

import (
	"fmt"
	"runtime"
)

const cleanOAuthCapsuleVersion = "openai-oauth-clean-profile-v1"

func requireCleanOAuthPlatform(goos string) error {
	if goos != "linux" {
		return fmt.Errorf("%s supports only linux; managed configuration fences are not implemented for %s", cleanOAuthCapsuleVersion, goos)
	}
	return nil
}

func requireCurrentCleanOAuthPlatform() error {
	return requireCleanOAuthPlatform(runtime.GOOS)
}
