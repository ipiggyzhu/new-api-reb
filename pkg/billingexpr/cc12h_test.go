package billingexpr

import "testing"

func TestCC12hCompilesAndCountsAsZero(t *testing.T) {
	expr := `len <= 272000 ? tier("0_272k", p * 10 + cr * 1 + cc * 12.5 + cc12h * 25 + c * 50) : tier("272k_plus", p * 20 + cr * 2 + cc * 25 + cc12h * 50 + c * 75)`
	got, trace, err := RunExpr(expr, TokenParams{P: 100, C: 10, Len: 100, CR: 20, CC: 4})
	if err != nil {
		t.Fatal(err)
	}
	if want := 100*10 + 20*1 + 4*12.5 + 10*50.0; got != want || trace.MatchedTier != "0_272k" {
		t.Fatalf("got %v tier %q, want %v", got, trace.MatchedTier, want)
	}
}
