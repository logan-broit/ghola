package secrets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScanner_DetectsSecrets(t *testing.T) {
	scanner := New()

	tests := []struct {
		name     string
		input    string
		wantRule string
	}{
		{
			name:     "OpenAI API key",
			input:    "The API key is sk-abcdefghijklmnopqrstuvwx",
			wantRule: "openai-api-key",
		},
		{
			name:     "OpenAI project key",
			input:    "sk-proj-abcdefghijklmnopqrstuvwxyz1234567890",
			wantRule: "openai-api-key",
		},
		{
			name:     "Anthropic API key",
			input:    "Use sk-ant-api03-abcdefghijklmnopqrstuvwxyz",
			wantRule: "anthropic-api-key",
		},
		{
			name:     "AWS access key",
			input:    "AKIAIOSFODNN7EXAMPLE is the key",
			wantRule: "aws-access-key",
		},
		{
			name:     "AWS secret key",
			input:    "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY12",
			wantRule: "aws-secret-key",
		},
		{
			name:     "Stripe secret key",
			input:    "sk_live_abcdefghijklmnopqrstuvwx",
			wantRule: "stripe-secret-key",
		},
		{
			name:     "Stripe restricted key",
			input:    "rk_live_abcdefghijklmnopqrstuvwx",
			wantRule: "stripe-restricted-key",
		},
		{
			name:     "GitHub PAT",
			input:    "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijkl",
			wantRule: "github-pat",
		},
		{
			name:     "GitHub OAuth token",
			input:    "gho_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijkl",
			wantRule: "github-oauth",
		},
		{
			name:     "GitHub fine-grained PAT",
			input:    "github_pat_11ABCDEFGH0123456789_abcdefghijklmnopqrstuvwxyz",
			wantRule: "github-fine-grained",
		},
		{
			name:     "RSA private key",
			input:    "-----BEGIN RSA PRIVATE KEY-----",
			wantRule: "private-key",
		},
		{
			name:     "EC private key",
			input:    "-----BEGIN EC PRIVATE KEY-----",
			wantRule: "private-key",
		},
		{
			name:     "Generic private key",
			input:    "-----BEGIN PRIVATE KEY-----",
			wantRule: "private-key",
		},
		{
			name:     "JWT token",
			input:    "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
			wantRule: "jwt",
		},
		{
			name:     "Postgres connection string",
			input:    "postgres://admin:secretpassword@db.example.com:5432/prod",
			wantRule: "database-url",
		},
		{
			name:     "MySQL connection string",
			input:    "mysql://root:p4ssw0rd@localhost:3306/mydb",
			wantRule: "database-url",
		},
		{
			name:     "MongoDB connection string",
			input:    "mongodb://user:pass1234@cluster0.example.net:27017/db",
			wantRule: "database-url",
		},
		{
			name:     "Password with equals",
			input:    `password = "supersecretpassword123"`,
			wantRule: "password-assignment",
		},
		{
			name:     "Password with colon",
			input:    `PASSWORD: 'myS3cur3P@ssw0rd!'`,
			wantRule: "password-assignment",
		},
		{
			name:     "Generic API key assignment",
			input:    "api_key=abcdefghijklmnopqrstuvwxyz1234",
			wantRule: "generic-api-key",
		},
		{
			name:     "Access token assignment",
			input:    `access_token: "abcdefghijklmnopqrstuvwxyz1234"`,
			wantRule: "generic-api-key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := scanner.Scan(tc.input)
			require.NotEmpty(t, findings, "expected at least one finding")

			ruleIDs := make([]string, len(findings))
			for i, f := range findings {
				ruleIDs[i] = f.RuleID
			}
			assert.Contains(t, ruleIDs, tc.wantRule)
		})
	}
}

func TestScanner_AllowsSafeContent(t *testing.T) {
	scanner := New()

	safeInputs := []struct {
		name  string
		input string
	}{
		{"plain text", "The database is PostgreSQL 15"},
		{"technical note", "We use AWS for hosting with IAM roles"},
		{"short password ref", "password is stored in vault"},
		{"code pattern", "func getAPIKey() string { return os.Getenv(\"API_KEY\") }"},
		{"url without credentials", "https://api.example.com/v1/status"},
		{"tag-like content", "[kubernetes,ssl] Use cert-manager for TLS"},
		{"postgres without creds", "postgres://localhost:5432/mydb"},
		{"mentions key generically", "Generate a new API key from the dashboard"},
		{"short token", "sk-short"},
		{"architecture note", "The service connects to Redis on port 6379"},
	}

	for _, tc := range safeInputs {
		t.Run(tc.name, func(t *testing.T) {
			findings := scanner.Scan(tc.input)
			assert.Empty(t, findings, "expected no findings for safe content, got: %v", findings)
		})
	}
}

func TestFormatError(t *testing.T) {
	findings := []Finding{
		{RuleID: "openai-api-key", Description: "OpenAI API key", Match: "sk-a****vwxy"},
		{RuleID: "aws-access-key", Description: "AWS access key ID", Match: "AKIA****MPLE"},
	}

	msg := FormatError(findings)
	assert.Contains(t, msg, "Memory rejected")
	assert.Contains(t, msg, "OpenAI API key")
	assert.Contains(t, msg, "AWS access key ID")
}

func TestFormatError_Empty(t *testing.T) {
	msg := FormatError(nil)
	assert.Empty(t, msg)
}

func TestRedact(t *testing.T) {
	assert.Equal(t, "****", redact("short"))
	assert.Equal(t, "****", redact("exactly12ch"))
	assert.Equal(t, "sk-a****wxyz", redact("sk-abcdefghijklmnopqrstuvwxyz"))
}
