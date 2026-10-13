package auth

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// metaQuotaRefusal is Meta's 429 when a subscription window runs out.
func metaQuotaRefusal(resetsAt time.Time) string {
	return fmt.Sprintf(`{"error":{"code":"rate_limit_exceeded","message":"Subscription quota exhausted. Your usage window resets at %s.","resets_at":%d,"type":"rate_limit_error"}}`,
		resetsAt.UTC().Format(time.RFC3339), resetsAt.Unix())
}

func TestModelCooldownError_PassesMetaQuotaRefusalThrough(t *testing.T) {
	t.Parallel()

	refusal := metaQuotaRefusal(time.Now().Add(45 * time.Minute))
	err := newModelCooldownErrorWithCause("muse-wonder-1.0-contributor", "", 45*time.Minute,
		&Error{Message: refusal, HTTPStatus: http.StatusTooManyRequests})

	if !err.DirectResponse() {
		t.Fatalf("DirectResponse() = false, want Meta's refusal passed through")
	}
	if got := string(err.ResponseBody()); got != refusal {
		t.Fatalf("ResponseBody() = %s, want %s", got, refusal)
	}
	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusTooManyRequests)
	}
	// Logs and Home dispatch keep reading the model_cooldown body.
	if got := gjson.Get(err.Error(), "error.code").String(); got != "model_cooldown" {
		t.Fatalf("Error() code = %q, want model_cooldown", got)
	}
}

func TestModelCooldownError_KeepsCooldownBodyWithoutAQuotaRefusal(t *testing.T) {
	t.Parallel()

	causes := map[string]error{
		"reset already passed": &Error{Message: metaQuotaRefusal(time.Now().Add(-time.Minute)), HTTPStatus: http.StatusTooManyRequests},
		"plain rate limit":     &Error{Message: `{"error":{"code":"rate_limit","message":"Rate limit exceeded."}}`, HTTPStatus: http.StatusTooManyRequests},
		"not a 429":            &Error{Message: metaQuotaRefusal(time.Now().Add(time.Hour)), HTTPStatus: http.StatusBadGateway},
		"text only":            errors.New("rate_limit_exceeded: Subscription quota exhausted."),
		"no cause":             nil,
	}
	for name, cause := range causes {
		err := newModelCooldownErrorWithCause("muse-wonder-1.0-contributor", "", time.Minute, cause)
		if err.DirectResponse() || err.ResponseBody() != nil {
			t.Errorf("%s: DirectResponse() = true, want the model_cooldown body", name)
		}
	}
}
