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
		CC: "4297690191832445|05|2027|832",
	})
	t.Logf("USER CARD TEST RESULT: %+v", res)
	if res.Status == "error" || res.Response == "CARD_DECLINED" && res.Message != "" && res.DeclineCode == "" && (res.Message == "This integration surface is unsupported for publishable key tokenization.") {
		t.Fatalf("Stripe card check failed with integration surface error: %s", res.Message)
	}
}

