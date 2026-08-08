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

func TestCheckStripeCustomSites(t *testing.T) {
	sites := []string{
		"https://www.charitywater.org/donate",
		"https://belovedcommunity.org/donate/",
	}

	for _, s := range sites {
		res := CheckStripe(StripeRequest{
			CC:   "4411050138393582|03|2031|309",
			Site: s,
		})
		t.Logf("CUSTOM SITE [%s] TEST RESULT: %+v", s, res)
		if res.Status == "error" {
			t.Errorf("Custom site %s check failed: %s", s, res.Message)
		}
	}
}

