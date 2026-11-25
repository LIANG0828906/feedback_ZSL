package engine

import (
	"fmt"

	"tlscheck/internal/model"
	"tlscheck/internal/projectio"
)

func ValidateChain(chain model.Chain, atMs int64, expectedPin string) ChainResult {
	result := ChainResult{ChainID: chain.ID}
	if err := projectio.ValidateChainStructure(chain); err != nil {
		result.Valid = false
		result.Levels = append(result.Levels, LevelResult{
			Index: 0, Code: "CHAIN_CYCLE", Reason: err.Error(), Passed: false,
		})
		return result
	}
	failed := false
	for i, cert := range chain.Certificates {
		result.TotalCostBP += cert.ValidationCostBP
		level := LevelResult{Index: i, Digest: cert.Digest, CostBP: cert.ValidationCostBP}
		switch {
		case failed:
			level.Code = "SKIPPED_AFTER_PRIOR_FAILURE"
			level.Reason = "later levels are not judged passed after an earlier failure"
			level.Skipped = true
			level.Passed = false
		case i == 0 && expectedPin != "" && cert.Digest != expectedPin:
			level.Code = "PIN_DIGEST_MISMATCH"
			level.Reason = fmt.Sprintf("pinned digest %s does not match leaf %s", expectedPin, cert.Digest)
			level.Passed = false
			failed = true
		case atMs < cert.NotBeforeMs:
			level.Code = "CERT_NOT_BEFORE_VALID"
			level.Reason = fmt.Sprintf("time %d is before not_before %d", atMs, cert.NotBeforeMs)
			level.Passed = false
			failed = true
		case atMs > cert.NotAfterMs:
			level.Code = "CERT_EXPIRED"
			level.Reason = fmt.Sprintf("time %d is after not_after %d", atMs, cert.NotAfterMs)
			level.Passed = false
			failed = true
		default:
			if i < len(chain.Certificates)-1 {
				next := chain.Certificates[i+1]
				if cert.Issuer != next.Subject {
					level.Code = "ISSUER_SUBJECT_MISMATCH"
					level.Reason = fmt.Sprintf("issuer %q does not match next subject %q", cert.Issuer, next.Subject)
					level.Passed = false
					failed = true
				}
			} else if cert.Issuer != cert.Subject {
				level.Code = "ROOT_ISSUER_MISMATCH"
				level.Reason = fmt.Sprintf("root issuer %q does not equal root subject %q", cert.Issuer, cert.Subject)
				level.Passed = false
				failed = true
			}
			if !failed {
				level.Code = "OK"
				level.Reason = "valid interval and issuer/subject relation"
				level.Passed = true
			}
		}
		result.Levels = append(result.Levels, level)
		if !level.Skipped {
			result.ChargedCostBP += level.CostBP
		}
	}
	result.Valid = !failed
	return result
}

func protocolFailure(req model.Request, ep model.Endpoint) string {
	if !projectio.ProtocolAllowed(req.OfferedProtocol, ep.MinProtocol, ep.MaxProtocol) {
		return HandshakeProtocolMismatch
	}
	if _, ok := projectio.CipherAllowed(req.OfferedCiphers, ep.CipherSuites); !ok {
		return HandshakeCipherMismatch
	}
	return ""
}
