package inbox

import "regexp"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`),
	regexp.MustCompile(`(?i)\baws_secret_access_key\s*[:=]\s*[A-Za-z0-9/+=]{30,}`),
	regexp.MustCompile(`(?i)\baccountkey\s*[:=]\s*[A-Za-z0-9/+=]{24,}`),
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{20,}=?`),
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|client[_-]?secret|password)\s*[:=]\s*["']?[A-Za-z0-9_./+=-]{12,}`),
}

func containsSecret(text string) bool {
	for _, pattern := range secretPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}
