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

func TestCheckStripeUserCard(t *testing.T) {
	res := CheckStripe(StripeRequest{
		CC:   "5131626807407898|05|2028|694",
		Site: "https://belovedcommunity.org/donate/",
	})
	t.Logf("BELOVEDCOMMUNITY CARD TEST RESULT: %+v", res)
}

