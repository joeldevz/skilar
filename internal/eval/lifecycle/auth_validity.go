package lifecycle

import (
	"fmt"
	"time"
)

const (
	openAIOAuthValiditySafetyMargin = 2 * time.Minute
	maxOpenAIOAuthRunHorizon        = 24 * time.Hour
)

func (s *OpenAIOAuthSession) ensureCredentialValidityLocked(minimumValidity time.Duration) error {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	return validateOpenAIOAuthValidity(s.credential, minimumValidity, now)
}

func validateOpenAIOAuthValidity(credential openAIOAuthCredential, minimumValidity time.Duration, now time.Time) error {
	if minimumValidity < 0 {
		return fmt.Errorf("OpenAI OAuth minimum validity must not be negative")
	}
	if minimumValidity > maxOpenAIOAuthRunHorizon {
		return fmt.Errorf("OpenAI OAuth minimum validity %s exceeds the supported 24h bound", minimumValidity)
	}
	requiredUntil := now.UTC().Add(minimumValidity + openAIOAuthValiditySafetyMargin)
	if credential.Expires <= requiredUntil.UnixMilli() {
		return fmt.Errorf("dedicated OpenAI OAuth credential does not cover the required run horizon; renew the clean profile before evaluation")
	}
	return nil
}
