package yunxiao

import (
	"errors"
	"strings"
	"testing"
)

// AC-6
func TestVerifySecret(t *testing.T) {
	const secret = "s3cret-value-0123456789"
	cases := []struct {
		name, secret, got string
		wantErr           bool
	}{
		{"missing_header", secret, "", true},
		{"wrong_value", secret, secret + "x", true},
		{"empty_secret", "", "", true},
		{"correct", secret, secret, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := VerifySecret(c.secret, c.got)
			if c.wantErr && !errors.Is(err, ErrBadSignature) {
				t.Errorf("got %v want ErrBadSignature", err)
			}
			if !c.wantErr && err != nil {
				t.Errorf("got %v want nil", err)
			}
		})
	}
}

// AC-6
func TestParseWorkitemID(t *testing.T) {
	cases := []struct {
		name, body, want string
		wantErr          bool
	}{
		{"empty", `{"id":""}`, "", true},
		{"too_long", `{"id":"` + strings.Repeat("a", 65) + `"}`, "", true},
		{"slash", `{"id":"a/b"}`, "", true},
		{"dotdot", `{"id":".."}`, "", true},
		{"missing", `{"subject":"x"}`, "", true},
		{"not_json", `not json`, "", true},
		{"valid", `{"id":"a1B2_c3-d4","subject":"伪造的标题","assignedTo":{"id":"x"}}`, "a1B2_c3-d4", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseWorkitemID([]byte(c.body))
			if c.wantErr {
				if !errors.Is(err, ErrBadPayload) {
					t.Errorf("got (%q, %v) want ErrBadPayload", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("got (%q, %v) want (%q, nil)", got, err, c.want)
			}
		})
	}
}
