package contracts

// These provenance extensions describe how the provider was authenticated and
// billed without recording a credential, account identifier, URL, or token.
// They are intentionally low-cardinality, fixed vocabulary values.
const (
	ProvenanceExtensionProviderAuthMode   = "x-provider-auth-mode"
	ProvenanceExtensionBillingMode        = "x-billing-mode"
	ProvenanceExtensionCredentialBoundary = "x-credential-boundary"
	ProvenanceExtensionAuthIsolation      = "x-auth-isolation"
	// ProvenanceExtensionProviderCatalogDigest binds a run to the exact
	// provider catalogue observed during the no-model runtime probe.
	ProvenanceExtensionProviderCatalogDigest = "x-effective-provider-catalog-digest"

	ProviderAuthModeOpenAIOAuthCleanProfileV1  = "openai-oauth-clean-profile-v1"
	BillingModeChatGPTSubscription             = "chatgpt-subscription"
	CredentialBoundaryRuntimeReadable          = "runtime-readable"
	CredentialBoundaryProviderProxy            = "provider-proxy"
	AuthIsolationDedicatedFreshTokenFailStopV1 = "dedicated-profile-fresh-token-fail-stop-v1"
)

// ProviderCostUSDAuthoritative reports whether a provider-reported USD value
// may be used as monetary evidence. ChatGPT subscription access is not billed
// per request, so an OpenCode cost value (including an explicit zero) is not an
// authoritative statement of spend in that mode. Independently calculated API
// pricing remains available through calculated_cost_usd as counterfactual
// evidence.
func (p Provenance) ProviderCostUSDAuthoritative() bool {
	if p.Extensions == nil {
		return true
	}
	// This v1 contract only defines extension values for subscription OAuth.
	// Fail closed for either key even when a caller invokes a metric extractor
	// before validating a malformed or future result.
	if _, present := p.Extensions[ProvenanceExtensionProviderAuthMode]; present {
		return false
	}
	if _, present := p.Extensions[ProvenanceExtensionBillingMode]; present {
		return false
	}
	return true
}

func validateProviderBillingProvenance(p Provenance) error {
	if p.Extensions == nil {
		return nil
	}
	if digest, present := p.Extensions[ProvenanceExtensionProviderCatalogDigest]; present && !validDigest(digest) {
		return fieldError("provenance.extensions."+ProvenanceExtensionProviderCatalogDigest, "must be a sha256 digest")
	}
	authMode, hasAuthMode := p.Extensions[ProvenanceExtensionProviderAuthMode]
	billingMode, hasBillingMode := p.Extensions[ProvenanceExtensionBillingMode]
	boundary, hasBoundary := p.Extensions[ProvenanceExtensionCredentialBoundary]
	isolation, hasIsolation := p.Extensions[ProvenanceExtensionAuthIsolation]
	if hasAuthMode {
		if authMode != ProviderAuthModeOpenAIOAuthCleanProfileV1 {
			return fieldError("provenance.extensions."+ProvenanceExtensionProviderAuthMode, "unsupported value %q", authMode)
		}
		if !hasBillingMode || billingMode != BillingModeChatGPTSubscription {
			return fieldError("provenance.extensions."+ProvenanceExtensionBillingMode, "must be %q for %s", BillingModeChatGPTSubscription, ProviderAuthModeOpenAIOAuthCleanProfileV1)
		}
		if !hasBoundary || (boundary != CredentialBoundaryRuntimeReadable && boundary != CredentialBoundaryProviderProxy) {
			return fieldError("provenance.extensions."+ProvenanceExtensionCredentialBoundary, "must describe the OAuth credential boundary")
		}
		if !hasIsolation || isolation != AuthIsolationDedicatedFreshTokenFailStopV1 {
			return fieldError("provenance.extensions."+ProvenanceExtensionAuthIsolation, "must be %q", AuthIsolationDedicatedFreshTokenFailStopV1)
		}
		if p.Provider != "openai" {
			return fieldError("provenance.provider", "must be openai for %s", ProviderAuthModeOpenAIOAuthCleanProfileV1)
		}
	}
	if hasBillingMode {
		if billingMode != BillingModeChatGPTSubscription {
			return fieldError("provenance.extensions."+ProvenanceExtensionBillingMode, "unsupported value %q", billingMode)
		}
		if !hasAuthMode || authMode != ProviderAuthModeOpenAIOAuthCleanProfileV1 {
			return fieldError("provenance.extensions."+ProvenanceExtensionProviderAuthMode, "must be %q for %s", ProviderAuthModeOpenAIOAuthCleanProfileV1, BillingModeChatGPTSubscription)
		}
	}
	if hasBoundary && !hasAuthMode {
		return fieldError("provenance.extensions."+ProvenanceExtensionProviderAuthMode, "is required with credential boundary")
	}
	if hasIsolation && !hasAuthMode {
		return fieldError("provenance.extensions."+ProvenanceExtensionProviderAuthMode, "is required with auth isolation")
	}
	return nil
}
