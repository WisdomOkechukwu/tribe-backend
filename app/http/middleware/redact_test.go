package middleware

import (
	"strings"
	"testing"
)

// TestRedactHidesSecretsAtAnyDepth checks nested objects and lists, which
// the old one-level regular expression could not see into.
func TestRedactHidesSecretsAtAnyDepth(test *testing.T) {
	body := `{"amount":"5","beneficiary":{"bank":"GTB","account_number":"0123456789"},"cards":[{"cvv":"123"}],"pin":"2580"}`
	logged := redact([]byte(body))
	for _, secret := range []string{"0123456789", "123\"", "2580"} {
		if strings.Contains(logged, secret) {
			test.Errorf("secret %q is visible in %s", secret, logged)
		}
	}
	if !strings.Contains(logged, `"bank":"GTB"`) || !strings.Contains(logged, `"amount":"5"`) {
		test.Errorf("ordinary fields should stay visible: %s", logged)
	}
}

// TestRedactBeforeCutting makes sure a long body is redacted BEFORE it is
// cut, so a secret near the cut is never half stored.
func TestRedactBeforeCutting(test *testing.T) {
	padding := strings.Repeat("x", loggedBodyLimit-20)
	body := `{"note":"` + padding + `","password":"SuperSecretValue123"}`
	logged := redact([]byte(body))
	if strings.Contains(logged, "SuperSecret") {
		test.Errorf("part of the password survived the cut: ...%s", logged[len(logged)-60:])
	}
	if len(logged) > loggedBodyLimit {
		test.Errorf("logged body is %d characters, limit is %d", len(logged), loggedBodyLimit)
	}
}

// TestRedactCutOffBody covers a body that is not valid JSON because it was
// cut off in the middle of a secret: the regular-expression fallback must
// still hide what is there.
func TestRedactCutOffBody(test *testing.T) {
	logged := redact([]byte(`{"amount":"5","verification_token":"abcdef0123`))
	if strings.Contains(logged, "abcdef") {
		test.Errorf("cut-off token is visible: %s", logged)
	}
}
