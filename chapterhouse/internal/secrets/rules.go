package secrets

import "regexp"

type rule struct {
	id          string
	description string
	pattern     *regexp.Regexp
}

func defaultRules() []rule {
	return []rule{
		// Provider API Keys
		{
			id:          "openai-api-key",
			description: "OpenAI API key",
			pattern:     regexp.MustCompile(`sk-[A-Za-z0-9\-]{20,}`),
		},
		{
			id:          "anthropic-api-key",
			description: "Anthropic API key",
			pattern:     regexp.MustCompile(`sk-ant-[A-Za-z0-9\-]{20,}`),
		},
		{
			id:          "aws-access-key",
			description: "AWS access key ID",
			pattern:     regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		},
		{
			id:          "aws-secret-key",
			description: "AWS secret access key",
			pattern:     regexp.MustCompile(`(?i)aws_secret_access_key\s*[=:]\s*[A-Za-z0-9/+=]{40}`),
		},
		{
			id:          "stripe-secret-key",
			description: "Stripe secret key",
			pattern:     regexp.MustCompile(`sk_live_[A-Za-z0-9]{24,}`),
		},
		{
			id:          "stripe-restricted-key",
			description: "Stripe restricted key",
			pattern:     regexp.MustCompile(`rk_live_[A-Za-z0-9]{24,}`),
		},
		{
			id:          "github-pat",
			description: "GitHub personal access token",
			pattern:     regexp.MustCompile(`ghp_[A-Za-z0-9]{36,}`),
		},
		{
			id:          "github-oauth",
			description: "GitHub OAuth access token",
			pattern:     regexp.MustCompile(`gho_[A-Za-z0-9]{36,}`),
		},
		{
			id:          "github-fine-grained",
			description: "GitHub fine-grained PAT",
			pattern:     regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		},
		// Private Keys
		{
			id:          "private-key",
			description: "Private key (PEM)",
			pattern:     regexp.MustCompile(`-----BEGIN\s+(RSA|EC|DSA|OPENSSH|PGP)?\s*PRIVATE KEY-----`),
		},
		// JWTs
		{
			id:          "jwt",
			description: "JSON Web Token",
			pattern:     regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		},
		// Database Connection Strings
		{
			id:          "database-url",
			description: "Database connection string with credentials",
			pattern:     regexp.MustCompile(`(?i)(postgres|mysql|mongodb|redis|amqp)://[^\s:]+:[^\s@]+@[^\s]+`),
		},
		// Password Assignments
		{
			id:          "password-assignment",
			description: "Password assignment",
			pattern:     regexp.MustCompile(`(?i)(password|passwd|pwd)\s*[=:]\s*['"][^\s'"]{8,}['"]`),
		},
		// Generic API Key Assignments
		{
			id:          "generic-api-key",
			description: "Generic API key assignment",
			pattern:     regexp.MustCompile(`(?i)(api[_-]?key|api[_-]?secret|access[_-]?token|auth[_-]?token)\s*[=:]\s*['"]?[A-Za-z0-9/+=_\-]{20,}['"]?`),
		},
	}
}
