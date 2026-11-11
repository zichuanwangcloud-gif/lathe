package tracker

import "testing"

// AC-12（tracker 部分）：云效进了平台取值域，其余取值不变。
func TestValidProvider(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"yunxiao", ProviderYunxiao, true},
		{"linear", ProviderLinear, true},
		{"internal", ProviderInternal, true},
		{"bogus", "bogus", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ValidProvider(c.in); got != c.want {
				t.Errorf("ValidProvider(%q) got %v want %v", c.in, got, c.want)
			}
		})
	}
}
