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

func TestCheckStripeProxy(t *testing.T) {
	res := CheckStripe(StripeRequest{
		CC:    "4411050138393582|03|2031|309",
		Proxy: "http://purevpn0s551451:9dpdlc2nfxgj@px023004.pointtoserver.com:10780",
	})
	t.Logf("PROXY TEST RESULT: %+v", res)
	if res.Status == "error" {
		t.Fatalf("Proxy Stripe check failed: %s", res.Message)
	}
}
