package budget

import "testing"

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		parts []string
		want  int
	}{
		{nil, 0},
		{[]string{""}, 0},
		{[]string{"abcd"}, 2},
		{[]string{"ab", "cd"}, 2},
		{[]string{"a"}, 1},
		{[]string{"ààà"}, 1}, // counted in runes, not bytes
		// Each invalid byte counts as one rune, as len([]rune(s)) does.
		{[]string{"\xff\xfe\xfd\xfc", "\xc3"}, 2},
	}
	for _, c := range cases {
		if got := EstimateTokens(c.parts...); got != c.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", c.parts, got, c.want)
		}
	}
}

func TestValidate(t *testing.T) {
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	if err := Validate(0, string(long)); err != nil {
		t.Errorf("maxTokens=0 must disable budgeting: %v", err)
	}
	if err := Validate(-5, string(long)); err != nil {
		t.Errorf("negative maxTokens must disable budgeting: %v", err)
	}
	if err := Validate(1000, "small"); err != nil {
		t.Errorf("under budget must pass: %v", err)
	}
	if err := Validate(10, string(long)); err == nil {
		t.Error("over budget must fail")
	}
	// estimate == max counts as exceeded
	if err := Validate(2, "abcd"); err == nil {
		t.Error("estimate equal to max must fail")
	}
}

func TestCheckTokens(t *testing.T) {
	if err := CheckTokens(0, 1_000_000); err != nil {
		t.Errorf("zero limit must disable the check: %v", err)
	}
	if err := CheckTokens(100, 99); err != nil {
		t.Errorf("under the limit must pass: %v", err)
	}
	if err := CheckTokens(100, 100); err == nil {
		t.Error("an estimate equal to the limit must fail")
	}
}
