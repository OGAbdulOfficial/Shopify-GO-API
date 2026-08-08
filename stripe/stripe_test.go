package stripe

import (
	"testing"
)

func TestCheckStripeDirect(t *testing.T) {
	res := CheckStripe(StripeRequest{
		CC: "4411050138393582|03|2031|309",
	})
	t.Logf("DIRECT TEST RESULT: %+v", res)
	if res.Status == "error" {
		t.Fatalf("Direct Stripe check failed: %s", res.Message)
	}
}

func TestCheckStripeDeclined(t *testing.T) {
	res := CheckStripe(StripeRequest{
		CC: "4000000000000002|03|2028|123",
	})
	t.Logf("DECLINED TEST RESULT: %+v", res)
}
